// gtb (Go tool builder) is a utility that builds tools written in Go.
//
// It reads a YAML configuration file listing Go module paths or git
// repositories, concurrently fetches and builds each one, and writes the
// resulting binaries to an output directory.
package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"sync/atomic"

	"github.com/schollz/progressbar/v3"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/load"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"
)

// config holds the parsed YAML configuration file.
type config struct {
	Tools map[string]Tool `yaml:"tools"`
}

// Tool describes a single tool entry in the configuration.
//
//   - Cmd overrides the output binary name (defaults to the last path element of
//     the module).
//   - Clone causes the repository to be fetched via git clone rather than go get.
//   - Deep disables the shallow-clone optimisation when Clone is set.
//   - Build lists shell commands to run inside the cloned directory; when empty,
//     a plain "go build" is used instead.
type Tool struct {
	Cmd   string
	Clone bool
	Deep  bool
	Build []string
}

// Builder manages the output directory and a shared temporary working
// directory used during builds.
type Builder struct {
	outputDir string
	tmpDir    string
}

var reModVersion = regexp.MustCompile(`^v\d+$`)

// buildStats tracks a rolling window of recent per-build durations for ETA estimation.
type buildStats struct {
	mu     sync.Mutex
	recent []time.Duration
}

// record adds a build duration to the rolling window (capped at 10 entries).
func (s *buildStats) record(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, d)
	if len(s.recent) > 10 {
		s.recent = s.recent[1:]
	}
}

// eta estimates remaining time given the number of items left and current
// concurrency. Returns 0 when there is not yet enough data.
func (s *buildStats) eta(remaining, concurrency int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.recent) == 0 || concurrency == 0 || remaining <= 1 {
		return 0
	}

	var total time.Duration

	for _, d := range s.recent {
		total += d
	}

	avg := total / time.Duration(len(s.recent))
	eff := concurrency
	if eff > remaining {
		eff = remaining
	}

	return avg * time.Duration(remaining) / time.Duration(eff)
}

// main initialises the CLI application and dispatches to toolBuild.
func main() {
	cwd, err := os.Getwd()
	if err != nil {
		log.Printf("getting current working directory: %s", err)
		os.Exit(1)
	}

	app := &cli.App{
		Name:   "gtb",
		Usage:  "Go tool builder",
		Action: toolBuild,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "config",
				Value: "config.yaml",
				Usage: "configuration filename",
			},
			&cli.StringFlag{
				Name:  "output-dir",
				Value: cwd,
				Usage: "output directory",
			},
			&cli.BoolFlag{
				Name:  "keep",
				Value: false,
				Usage: "keep build directory",
			},
			&cli.Int64Flag{
				Name:  "max-concurrency",
				Value: 0,
				Usage: "maximum number of build processes to run in parallel, 0 means available CPUs",
			},
		},
	}

	err = app.Run(os.Args)
	if err != nil {
		log.Printf("E: %s", err)
		os.Exit(1)
	}
}

// toolBuild loads configuration, creates a Builder, and concurrently builds
// every requested tool while throttling parallelism based on system load.
//
// At most 4 build goroutines run simultaneously, and new ones are held back
// until the 1-minute load average drops below twice the logical CPU count.
func toolBuild(c *cli.Context) error {
	cfg, err := getConfig(c.String("config"))
	if err != nil {
		return fmt.Errorf("getting configuration: %w", err)
	}

	builder, err := newBuilder(c.String("output-dir"))
	if err != nil {
		return fmt.Errorf("creating builder: %w", err)
	}

	if !c.Bool("keep") {
		defer builder.Cleanup()
	}

	work := getWork(cfg.Tools, c.Args().Slice())

	progress := progressbar.NewOptions(len(work),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionSetElapsedTime(true),
		progressbar.OptionShowCount(),
		progressbar.OptionFullWidth(),
	)

	cpus, err := cpu.Counts(true)
	if err != nil {
		return fmt.Errorf("getting CPU counts: %w", err)
	}

	maxRunning := int32(cpus)

	if maxConcurrency := c.Int64("max-concurrency"); maxConcurrency != 0 {
		maxRunning = int32(maxConcurrency)
	}

	var (
		waitgroup sync.WaitGroup
		running   int32
		completed int64
		stats     buildStats
	)

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r := int(atomic.LoadInt32(&running))
				rem := len(work) - int(atomic.LoadInt64(&completed))
				eta := stats.eta(rem, r)
				desc := fmt.Sprintf("(%d running", r)
				if eta > 0 {
					desc += fmt.Sprintf(", ~%s left", eta.Round(time.Second))
				}
				desc += ")"
				progress.Describe(desc)

			case <-done:
				progress.Describe("(done, cleaning up...)")
				return
			}
		}
	}()

	for name, toolcfg := range work {
		for {
			avg, err := load.Avg()
			if err != nil {
				return fmt.Errorf("reading load average: %w", err)
			}

			if avg.Load1 <= 2*float64(cpus) && atomic.LoadInt32(&running) < maxRunning {
				break
			} else {
				time.Sleep(50 * time.Millisecond)
			}
		}

		atomic.AddInt32(&running, 1)
		waitgroup.Add(1)

		go func(name string, toolcfg Tool) {
			start := time.Now()
			err := builder.Build(name, toolcfg)
			if err != nil {
				log.Printf("W: building tool %s: %s", name, err)
			}

			stats.record(time.Since(start))
			atomic.AddInt64(&completed, 1)
			_ = progress.Add(1)

			waitgroup.Done()
			atomic.AddInt32(&running, -1)
		}(name, toolcfg)
	}

	waitgroup.Wait()
	close(done)

	return nil
}

// getConfig reads and unmarshals the YAML configuration file at fn.
func getConfig(fn string) (*config, error) {
	buf, err := os.ReadFile(fn)
	if err != nil {
		return nil, fmt.Errorf("reading configuration file: %w", err)
	}

	var cfg config

	err = yaml.Unmarshal(buf, &cfg)
	if err != nil {
		return nil, fmt.Errorf("reading configuration: %w", err)
	}

	return &cfg, nil
}

// getWork returns the subset of tools matching the names in args.
//
// When args is empty the full tools map is returned unchanged so that all
// configured tools are built.
func getWork(tools map[string]Tool, args []string) map[string]Tool {
	if len(args) == 0 {
		return tools
	}

	want := map[string]bool{}

	for _, tool := range args {
		want[tool] = true
	}

	for name := range tools {
		if !want[name] {
			delete(tools, name)
		}
	}

	return tools
}

// basename returns the binary name derived from a Go module path.
//
// It handles versioned module suffixes of the form ".../vN" by stripping the
// version segment and returning the preceding path element instead.
func basename(name string) string {
	out := path.Base(name)

	// take into account modules that look like
	// example.org/something/name/vN
	if !reModVersion.MatchString(out) {
		return out
	}

	return path.Base(path.Dir(name))
}

// newBuilder creates a Builder whose temporary working directory is a new
// subdirectory of outputDir.
func newBuilder(outputDir string) (*Builder, error) {
	tmpdir, err := os.MkdirTemp(outputDir, "build-")
	if err != nil {
		return nil, fmt.Errorf("creating temporary directory: %w", err)
	}

	return &Builder{
		outputDir: outputDir,
		tmpDir:    tmpdir,
	}, nil
}

// Cleanup removes the temporary build directory created by newBuilder.
func (b *Builder) Cleanup() {
	err := os.RemoveAll(b.tmpDir)
	if err != nil {
		log.Printf("W: cleaning up: %s", err.Error())
	}
}

// Build builds a single tool identified by mod using the settings in toolcfg.
//
// It creates an isolated subdirectory under the shared temporary directory,
// resolves the output binary name, and delegates to cloneAndBuild or
// goGetAndBuild depending on whether toolcfg.Clone is set.
func (b *Builder) Build(mod string, toolcfg Tool) error {
	output := toolcfg.Cmd
	if len(output) == 0 {
		output = basename(mod)
	} else if filepath.Base(output) != output {
		return fmt.Errorf("invalid cmd: %s", output)
	}

	buildFunc := b.goGetAndBuild

	if toolcfg.Clone {
		buildFunc = b.cloneAndBuild
	}

	tmpdir, err := os.MkdirTemp(b.tmpDir, output+"-")
	if err != nil {
		return fmt.Errorf("creating temporary directory: %w", err)
	}

	outputPath := filepath.Join(b.outputDir, output)

	stdout := bytes.NewBuffer(nil)
	stderr := bytes.NewBuffer(nil)

	return buildFunc(toolcfg, mod, tmpdir, outputPath, stdout, stderr)
}

// expandVars splits cmd on whitespace and expands $OUTDIR in each argument to
// the builder's output directory.
//
// Note: the splitting is intentionally naive and does not handle quoted tokens
// or other shell metacharacters.
func (b *Builder) expandVars(cmd string) []string {
	// TODO(mem): this is oh, so wrong...
	// https://github.com/gopherclass/go-shellquote might be useful.
	args := strings.Split(cmd, " ")
	for i := range args[1:] {
		args[i+1] = os.Expand(args[i+1], func(key string) string {
			switch key {
			case "OUTDIR":
				return b.outputDir
			default:
				return ""
			}
		})
	}

	return args
}

// latestTag queries the remote repository at url for its most recent tag using
// git ls-remote and returns the tag name (e.g. "v0.50.18"). Returns an empty
// string without error when the repository has no tags.
func latestTag(url string, stderr *bytes.Buffer) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "ls-remote", "--tags", "--refs", "--sort=version:refname", url)
	cmd.Stdout = &out
	cmd.Stderr = stderr

	if err := run(cmd, &out, stderr); err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			return strings.TrimPrefix(parts[1], "refs/tags/"), nil
		}
	}
	return "", nil
}

// cloneAndBuild clones mod as a git repository into tmpdir at the most recent
// tagged release, and either runs the custom build steps in toolcfg.Build or
// falls back to "go build" when none are provided.
//
// The latest tag is resolved via git ls-remote before cloning. A shallow clone
// (--depth 1) is used by default; setting toolcfg.Deep performs a full clone.
// If the repository has no tags, the default branch is cloned instead.
func (b *Builder) cloneAndBuild(toolcfg Tool, mod, tmpdir, outputPath string, stdout, stderr *bytes.Buffer) error {
	url := "https://" + mod

	tag, err := latestTag(url, stderr)
	if err != nil {
		return fmt.Errorf("listing tags for %s: %w", mod, err)
	}

	var gitArgs []string
	switch {
	case tag != "" && !toolcfg.Deep:
		gitArgs = []string{"clone", "--depth", "1", "--branch", tag, url, tmpdir}
	case tag != "" && toolcfg.Deep:
		gitArgs = []string{"clone", "--branch", tag, url, tmpdir}
	case toolcfg.Deep:
		gitArgs = []string{"clone", url, tmpdir}
	default:
		gitArgs = []string{"clone", "--depth", "1", url, tmpdir}
	}

	gitCmd := exec.Command("git", gitArgs...)
	gitCmd.Stdout = stdout
	gitCmd.Stderr = stderr

	if err := run(gitCmd, stdout, stderr); err != nil {
		return fmt.Errorf("cloning %s: %w", mod, err)
	}

	if len(toolcfg.Build) > 0 {
		for _, step := range toolcfg.Build {
			args := b.expandVars(step)

			cmd := exec.Command(args[0], args[1:]...)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			cmd.Dir = tmpdir

			err := run(cmd, stdout, stderr)
			if err != nil {
				return err
			}

			if !cmd.ProcessState.Success() {
				log.Printf("W: running step %q: %s", step, err)
				log.Printf("W: stdout: %s", stdout.String())
				log.Printf("W: stderr: %s", stderr.String())

				return fmt.Errorf("command %q failed", step)
			}
		}
	} else {
		// try go build
		buildCmd := exec.Command("go", "build", "-o", outputPath, mod)
		buildCmd.Stdout = stdout
		buildCmd.Stderr = stderr
		buildCmd.Dir = tmpdir

		stdout.WriteString(buildCmd.String())
		stdout.WriteRune('\n')

		if err := run(buildCmd, stdout, stderr); err != nil {
			return err
		}
	}

	return nil
}

// run executes cmd and returns its error.
//
// On failure it logs the command string together with the captured stdout and
// stderr buffers to aid debugging.
func run(cmd *exec.Cmd, stdout, stderr fmt.Stringer) error {
	err := cmd.Run()
	if err != nil {
		log.Printf("W: running command %s: %s", cmd.String(), err)
		log.Printf("W: stdout: %s", stdout.String())
		log.Printf("W: stderr: %s", stderr.String())
	}

	return err
}

// goGetAndBuild fetches mod into a fresh temporary module via "go get" and
// then compiles it with "go build", writing the binary to outputPath.
func (b *Builder) goGetAndBuild(toolcfg Tool, mod, tmpdir, outputPath string, stdout, stderr *bytes.Buffer) error {
	if err := createGoMod(tmpdir); err != nil {
		return fmt.Errorf("creating temporary go.mod: %w", err)
	}

	getCmd := exec.Command("go", "get", mod)
	getCmd.Stdout = stdout
	getCmd.Stderr = stderr
	getCmd.Dir = tmpdir

	stdout.WriteString(getCmd.String())
	stdout.WriteRune('\n')

	err := getCmd.Run()
	if err != nil {
		log.Printf("W: getting %s: %s", mod, err)
		log.Printf("W: stdout: %s", stdout.String())
		log.Printf("W: stderr: %s", stderr.String())

		return err
	}

	buildCmd := exec.Command("go", "build", "-o", outputPath, mod)
	buildCmd.Stdout = stdout
	buildCmd.Stderr = stderr
	buildCmd.Dir = tmpdir

	stdout.WriteString(buildCmd.String())
	stdout.WriteRune('\n')

	err = buildCmd.Run()
	if err != nil {
		log.Printf("W: building %s: %s", mod, err)
		log.Printf("W: stdout: %s", stdout.String())
		log.Printf("W: stderr: %s", stderr.String())

		return err
	}

	return nil
}

// createGoMod writes a minimal go.mod file declaring module name "tmp" to dir.
//
// This synthetic module is used as a throwaway workspace by goGetAndBuild so
// that "go get" and "go build" can be run outside of any real module.
func createGoMod(dir string) error {
	fn := filepath.Join(dir, "go.mod")

	fh, err := os.Create(fn)
	if err != nil {
		return fmt.Errorf("creating go.mod: %w", err)
	}

	defer func() {
		err := fh.Close()
		if err != nil {
			log.Printf("W: closing module file %s: %s", fn, err.Error())
		}
	}()

	_, err = fmt.Fprintf(fh, "module tmp\n")
	if err != nil {
		return fmt.Errorf("writing to go.mod: %w", err)
	}

	return nil
}

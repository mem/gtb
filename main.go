package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

type config struct {
	Tools map[string]Tool `yaml:"tools"`
}

type Tool struct {
	Cmd   string
	Clone bool
	Build []string
}

type Builder struct {
	outputDir string
	tmpDir    string
}

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
		},
	}

	err = app.Run(os.Args)
	if err != nil {
		log.Printf("E: %s", err)
		os.Exit(1)
	}
}

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

	progress := progressbar.Default(int64(len(work)))

	cpus, err := cpu.Counts(true)
	if err != nil {
		return fmt.Errorf("getting CPU counts: %w", err)
	}

	var (
		waitgroup sync.WaitGroup
		running   int32
	)

	for name, toolcfg := range work {
		for {
			avg, err := load.Avg()
			if err != nil {
				return fmt.Errorf("reading load average: %w", err)
			}

			if avg.Load1 <= 2*float64(cpus) && atomic.LoadInt32(&running) < 4 {
				break
			} else {
				time.Sleep(50 * time.Millisecond)
			}
		}

		atomic.AddInt32(&running, 1)
		waitgroup.Add(1)

		go func(name string, toolcfg Tool) {
			err := builder.Build(name, toolcfg)
			if err != nil {
				log.Printf("W: building tool %s: %s", name, err)
			}

			_ = progress.Add(1)

			waitgroup.Done()
			atomic.AddInt32(&running, -1)
		}(name, toolcfg)
	}

	waitgroup.Wait()

	return nil
}

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

func basename(name string) string {
	parts := strings.Split(name, "/")

	return parts[len(parts)-1]
}

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

func (b *Builder) Cleanup() {
	os.RemoveAll(b.tmpDir)
}

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

func (b *Builder) cloneAndBuild(toolcfg Tool, mod, tmpdir, outputPath string, stdout, stderr *bytes.Buffer) error {
	gitCmd := exec.Command("git", "clone", "--depth", "1", "https://"+mod, tmpdir)
	gitCmd.Stdout = stdout
	gitCmd.Stderr = stderr

	err := gitCmd.Run()
	if err != nil {
		log.Printf("W: cloning %s: %s", mod, err)
		log.Printf("W: stdout: %s", stdout.String())
		log.Printf("W: stderr: %s", stderr.String())

		return fmt.Errorf("cloning %s: %w", mod, err)
	}

	if len(toolcfg.Build) > 0 {
		for _, step := range toolcfg.Build {
			args := b.expandVars(step)

			cmd := exec.Command(args[0], args[1:]...)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			cmd.Dir = tmpdir

			if err := cmd.Run(); err != nil {
				log.Printf("W: running step %q: %s", step, err)
				log.Printf("W: stdout: %s", stdout.String())
				log.Printf("W: stderr: %s", stderr.String())

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

		err = buildCmd.Run()
		if err != nil {
			log.Printf("W: building %s: %s", mod, err)
			log.Printf("W: stdout: %s", stdout.String())
			log.Printf("W: stderr: %s", stderr.String())

			return err
		}
	}

	return nil
}

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

func createGoMod(dir string) error {
	fn := filepath.Join(dir, "go.mod")

	fh, err := os.Create(fn)
	if err != nil {
		return fmt.Errorf("creating go.mod: %w", err)
	}

	defer fh.Close()

	_, err = fmt.Fprintf(fh, "module tmp\n")
	if err != nil {
		return fmt.Errorf("writing to go.mod: %w", err)
	}

	return nil
}

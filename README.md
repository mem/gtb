Go tool build
=============

gtb (Go tool build) builds Go tools, e.g. goimports, golangci-lint, etc.

Originally I did this using a shell script, but over time the number of
variations and cases that need to be considered have grown quite large.
The structure of the program reflects this origin: this is more or less
a shell script translated into Go.

The configuration file lists all tools that should be built. See the
example file [tools.yaml](tools.yaml). The only required information is
the Go path of the corresponding tool.  Optionally, a Git repository can
be cloned and build instructions can be added, e.g. in order to build
`k3d` this is enough:

  github.com/rancher/k3d:
    clone: true
    build:
     - make build BINDIR=${OUTDIR}

`OUTDIR` is a variable that is expanded to the name of the output
directory.

A complete example:

```yaml
tools:
  github.com/golangci/golangci-lint/cmd/golangci-lint:
  github.com/hairyhenderson/gomplate/v3/cmd/gomplate:
  github.com/rancher/k3d:
    clone: true
    build:
     - make build BINDIR=${OUTDIR}
```

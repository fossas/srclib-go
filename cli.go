package main

import (
	"encoding/json"
	"fmt"

	"go/build"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/sourcegraph/srclib/dep2"
	"github.com/sourcegraph/srclib/toolchain"
	"github.com/sourcegraph/srclib/unit"
)

var (
	parser = flags.NewNamedParser("srclib-go", flags.Default)
	cwd    string
)

func init() {
	parser.LongDescription = "srclib-go performs Go package, dependency, and source analysis."

	var err error
	cwd, err = os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	log.SetFlags(0)
	if _, err := parser.Parse(); err != nil {
		os.Exit(1)
	}
}

func init() {
	_, err := parser.AddCommand("scan",
		"scan for Go packages",
		"Scan the directory tree rooted at the current directory for Go packages.",
		&scanCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type ScanCmd struct {
	Repo   string `long:"repo" description:"repository URI" value-name:"URI"`
	Subdir string `long:"subdir" description:"subdirectory in repository" value-name:"DIR"`
}

var scanCmd ScanCmd

func (c *ScanCmd) Execute(args []string) error {
	if c.Repo == "" && os.Getenv("IN_DOCKER_CONTAINER") != "" {
		log.Println("Warning: no --repo specified, and tool is running in a Docker container (i.e., without awareness of host's GOPATH). Go import paths in source units produced by the scanner may be inaccurate. To fix this, ensure that the --repo URI is specified. Report this issue if you are seeing it unexpectedly.")
	}

	cmd := exec.Command("go", "list", "-e", "-json", "./...")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	dec := json.NewDecoder(stdout)
	var units []*unit.SourceUnit
	for {
		var pkg *build.Package
		if err := dec.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		pv, pt := reflect.ValueOf(pkg).Elem(), reflect.TypeOf(*pkg)

		// collect all files
		var files []string
		for i := 0; i < pt.NumField(); i++ {
			f := pt.Field(i)
			if strings.HasSuffix(f.Name, "Files") {
				fv := pv.Field(i).Interface()
				files = append(files, fv.([]string)...)
			}
		}

		// collect all imports
		depsMap := map[string]struct{}{}
		for i := 0; i < pt.NumField(); i++ {
			f := pt.Field(i)
			if strings.HasSuffix(f.Name, "Imports") {
				fv := pv.Field(i).Interface()
				imports := fv.([]string)
				for _, imp := range imports {
					depsMap[imp] = struct{}{}
				}
			}
		}
		deps0 := make([]string, len(depsMap))
		i := 0
		for imp := range depsMap {
			deps0[i] = imp
			i++
		}
		sort.Strings(deps0)
		deps := make([]interface{}, len(deps0))
		for i, imp := range deps0 {
			deps[i] = imp
		}

		// make all dirs relative to the current dir
		for i := 0; i < pt.NumField(); i++ {
			f := pt.Field(i)
			if strings.HasSuffix(f.Name, "Dir") {
				fv := pv.Field(i)
				dir := fv.Interface().(string)
				if dir != "" {
					dir, err := filepath.Rel(cwd, dir)
					if err != nil {
						return err
					}
					fv.Set(reflect.ValueOf(dir))
				}
			}
		}

		// fix up import path to be consistent when running as a program and as
		// a Docker container.
		pkg.ImportPath = filepath.Join(c.Repo, c.Subdir, pkg.Dir)

		units = append(units, &unit.SourceUnit{
			Name:         pkg.ImportPath,
			Type:         "GoPackage",
			Files:        files,
			Data:         pkg,
			Dependencies: deps,
			Ops:          map[string]*toolchain.ToolRef{"depresolve": nil, "graph": nil},
		})
	}
	if err := cmd.Wait(); err != nil {
		return err
	}

	if err := json.NewEncoder(os.Stdout).Encode(units); err != nil {
		return err
	}
	return nil
}

func init() {
	_, err := parser.AddCommand("depresolve",
		"resolve a Go package's imports",
		"Resolve a Go package's imports to their repository clone URL.",
		&depResolveCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type DepResolveCmd struct{}

var depResolveCmd DepResolveCmd

func (c *DepResolveCmd) Execute(args []string) error {
	var unit *unit.SourceUnit
	if err := json.NewDecoder(os.Stdin).Decode(&unit); err != nil {
		return err
	}
	if err := os.Stdin.Close(); err != nil {
		return err
	}

	res := make([]*dep2.Resolution, len(unit.Dependencies))
	for i, dep := range unit.Dependencies {
		res[i] = &dep2.Resolution{Error: fmt.Sprintf("TODO %v", dep)}
	}

	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		return err
	}
	return nil
}

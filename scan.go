package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/build"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sourcegraph.com/sourcegraph/srclib"
	"sourcegraph.com/sourcegraph/srclib/unit"
	"sourcegraph.com/sourcegraph/srclib-go/gog"
)

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

type ScanCmd struct{}

var scanCmd ScanCmd

func (c *ScanCmd) Execute(args []string) error {
	if err := json.NewDecoder(os.Stdin).Decode(&config); err != nil {
		return err
	}
	if err := os.Stdin.Close(); err != nil {
		return err
	}

	// Automatically detect vendored dirs (check for vendor/src and
	// Godeps/_workspace/src) and set up GOPATH pointing to them if
	// they exist.
	//
	// Note that the `vendor` directory here is used by 3rd party vendoring
	// tools and is NOT the `vendor` directory in the Go 1.6 official vendor
	// specification (that `vendor` directory does not have a `src`
	// subdirectory).
	var setAutoGOPATH bool
	if config.GOPATH == "" {
		vendorDirs := []string{"vendor", "Godeps/_workspace"}
		var foundGOPATHs []string
		for _, vdir := range vendorDirs {
			if fi, err := os.Stat(filepath.Join(cwd, vdir, "src")); err == nil && fi.Mode().IsDir() {
				foundGOPATHs = append(foundGOPATHs, vdir)
				setAutoGOPATH = true
				log.Printf("Adding %s to GOPATH (auto-detected Go vendored dependencies source dir %s). If you don't want this, make a Srcfile with a GOPATH property set to something other than the empty string.", vdir, filepath.Join(vdir, "src"))
			}
		}
		config.GOPATH = strings.Join(foundGOPATHs, string(filepath.ListSeparator))
	}

	if err := config.apply(); err != nil {
		return err
	}

	scanDir, err := filepath.EvalSymlinks(getCWD())
	if err != nil {
		return err
	}

	units, err := scan(scanDir)
	if err != nil {
		return err
	}

	if len(config.PkgPatterns) != 0 {
		matchers := make([]func(name string) bool, len(config.PkgPatterns))
		for i, pattern := range config.PkgPatterns {
			matchers[i] = matchPattern(pattern)
		}

		var filteredUnits []*unit.SourceUnit
		for _, unit := range units {
			for _, m := range matchers {
				if m(unit.Name) {
					filteredUnits = append(filteredUnits, unit)
					break
				}
			}
		}
		units = filteredUnits
	}

	// Make vendored dep unit names (package import paths) relative to
	// vendored src dir, not to top-level dir.
	if config.GOPATH != "" {
		dirs := filepath.SplitList(config.GOPATH)
		for _, dir := range dirs {
			relDir, err := filepath.Rel(cwd, dir)
			if err != nil {
				return err
			}
			srcDir := filepath.Join(relDir, "src")
			for _, u := range units {
				pkg := u.Data.(*build.Package)
				if strings.HasPrefix(pkg.Dir, srcDir) {
					relImport, err := filepath.Rel(srcDir, pkg.Dir)
					if err != nil {
						return err
					}
					pkg.ImportPath = relImport
					u.Name = pkg.ImportPath
				}
			}
		}
	}

	// Make go1.5 style vendored dep unit names (package import paths)
	// relative to vendored dir, not to top-level dir.
	for _, u := range units {
		if name, isVendored := vendoredUnitName(u.Data.(*build.Package)); isVendored {
			u.Name = name
		}
	}

	// make files relative to repository root
	for _, u := range units {
		pkgSubdir := u.Data.(*build.Package).Dir
		for i, f := range u.Files {
			u.Files[i] = filepath.ToSlash(filepath.Join(pkgSubdir, f))
		}
	}

	// If we automatically set the GOPATH based on the presence of
	// vendor dirs, then we need to pass the GOPATH to the units
	// because it is not persisted in the Srcfile. Otherwise the other
	// tools would never see the auto-set GOPATH.
	if setAutoGOPATH {
		for _, u := range units {
			if u.Config == nil {
				u.Config = map[string]interface{}{}
			}

			dirs := filepath.SplitList(config.GOPATH)
			for i, dir := range dirs {
				relDir, err := filepath.Rel(cwd, dir)
				if err != nil {
					return err
				}
				dirs[i] = relDir
			}
			u.Config["GOPATH"] = strings.Join(dirs, string(filepath.ListSeparator))
		}
	}

	// Find vendored units to build a list of vendor directories
	vendorDirs := map[string]struct{}{}
	for _, u := range units {
		i, ok := findVendor(u.Dir)
		// Don't include old style vendor dirs
		if !ok || strings.HasPrefix(u.Dir[i:], "vendor/src/") {
			continue
		}
		vendorDirs[u.Dir[:i+len("vendor")]] = struct{}{}
	}

	for _, u := range units {
		unitDir := u.Dir + string(filepath.Separator)
		var dirs vendorDirSlice
		for dir := range vendorDirs {
			// Must be a child of baseDir to use the vendor dir
			baseDir := filepath.Dir(dir) + string(filepath.Separator)
			if filepath.Clean(baseDir) == "." || strings.HasPrefix(unitDir, baseDir) {
				dirs = append(dirs, dir)
			}
		}
		sort.Sort(dirs)
		if len(dirs) > 0 {
			if u.Config == nil {
				u.Config = map[string]interface{}{}
			}
			u.Config["VendorDirs"] = dirs
		}
	}

	b, err := json.MarshalIndent(units, "", "  ")
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(b); err != nil {
		return err
	}
	return nil
}

// findVendor from golang/go/cmd/go/pkg.go
func findVendor(path string) (index int, ok bool) {
	// Two cases, depending on internal at start of string or not.
	// The order matters: we must return the index of the final element,
	// because the final one is where the effective import path starts.
	switch {
	case strings.Contains(path, "/vendor/"):
		return strings.LastIndex(path, "/vendor/") + 1, true
	case strings.HasPrefix(path, "vendor/"):
		return 0, true
	}
	return 0, false
}

// vendoredUnitName returns the proper unit name of a Go package if it
// is vendored. If the package is not vendored, it returns the empty
// string and false.
func vendoredUnitName(pkg *build.Package) (name string, isVendored bool) {
	i, ok := findVendor(pkg.Dir)
	if ok {
		relDir := pkg.Dir[i+len("vendor"):]
		if strings.HasPrefix(relDir, "/src/") || !strings.HasPrefix(relDir, "/") {
			return "", false
		}
		relImport := relDir[1:]
		return relImport, true
	} else {
		i, ok := findGoDeps(pkg.Dir)
		if !ok {
			return "", false
		}
		relDir := pkg.Dir[i+len("Godeps/_workspace/src"):]
		if strings.HasPrefix(relDir, "/src/") || !strings.HasPrefix(relDir, "/") {
			return "", false
		}
		relImport := relDir[1:]
		return relImport, true
	}
}

// Copied from findVendor
func findGoDeps(path string) (index int, ok bool) {
	switch {
	case strings.Contains(path, "/Godeps/_workspace/src/"):
		return strings.LastIndex(path, "/Godeps/_workspace/src/") + 1, true
	case strings.HasPrefix(path, "Godeps/_workspace/src/"):
		return 0, true
	}
	return 0, false
}

func isInGopath(path string) bool {
	for _, gopath := range filepath.SplitList(buildContext.GOPATH) {
		if strings.HasPrefix(evalSymlinks(path), filepath.Join(evalSymlinks(gopath), "src")) {
			return true
		}
	}
	return false
}

func scan(scanDir string) ([]*unit.SourceUnit, error) {
	// TODO(sqs): include xtest, but we'll have to make them have a distinctly
	// namespaced def path from the non-xtest pkg.

	pkgs, err := scanForPackages(scanDir, scanDir)
	if err != nil {
		return nil, err
	}

	var units []*unit.SourceUnit
	for _, pkg := range pkgs {
		// Collect all files
		var files []string
		files = append(files, pkg.GoFiles...)
		files = append(files, pkg.CgoFiles...)
		files = append(files, pkg.IgnoredGoFiles...)
		files = append(files, pkg.CFiles...)
		files = append(files, pkg.CXXFiles...)
		files = append(files, pkg.MFiles...)
		files = append(files, pkg.HFiles...)
		files = append(files, pkg.SFiles...)
		files = append(files, pkg.SwigFiles...)
		files = append(files, pkg.SwigCXXFiles...)
		files = append(files, pkg.SysoFiles...)
		files = append(files, pkg.TestGoFiles...)
		files = append(files, pkg.XTestGoFiles...)

		// Collect all imports. We use a map to remove duplicates.
		var imports []string
		imports = append(imports, pkg.Imports...)
		imports = append(imports, pkg.TestImports...)
		imports = append(imports, pkg.XTestImports...)
		imports = uniq(imports)
		sort.Strings(imports)

		// Create appropriate type for (unit).SourceUnit
		deps := make([]interface{}, len(imports))
		for i, imp := range imports {
			deps[i] = imp
		}

		pkg.Dir, err = filepath.Rel(scanDir, pkg.Dir)
		if err != nil {
			return nil, err
		}
		pkg.Dir = filepath.ToSlash(pkg.Dir)
		pkg.BinDir = ""
		pkg.ConflictDir = ""

		// Root differs depending on the system, so it's hard to compare results
		// across environments (when running as a program). Clear it so we can
		// compare results in tests more easily.
		pkg.Root = ""
		pkg.SrcRoot = ""
		pkg.PkgRoot = ""

		pkg.ImportPos = nil
		pkg.TestImportPos = nil
		pkg.XTestImportPos = nil

		units = append(units, &unit.SourceUnit{
			Name:         pkg.ImportPath,
			Type:         "GoPackage",
			Dir:          pkg.Dir,
			Files:        files,
			Data:         pkg,
			Dependencies: deps,
			Ops:          map[string]*srclib.ToolRef{"depresolve": nil, "graph-all": nil},
		})
	}

	assignGitCommits(scanDir, units)
	assignGodepsCommits(scanDir, units)
	assignGovendorCommits(scanDir, units)
	assignRevisionsToDependencies(units)
	units = filterVendorizedDependencies(units)

	return units, nil
}

func filterVendorizedDependencies(units []*unit.SourceUnit) ([]*unit.SourceUnit) {
	newUnits := make([]*unit.SourceUnit, 0)
	for _, unit := range units {
		if _, isVendored := vendoredUnitName(unit.Data.(*build.Package)); !isVendored {
			newUnits = append(newUnits, unit)
		}
	}
	return newUnits
}

func assignGitCommits(dir string, units []*unit.SourceUnit) {
	_, err := exec.LookPath("git")
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"go: missing %s command. See https://golang.org/s/gogetcmd\n",
			"git")
		return
	}

	var buf bytes.Buffer
	for _, unit := range units {
		if _, isVendored := vendoredUnitName(unit.Data.(*build.Package)); !isVendored {
			fullpath := filepath.Join(dir, unit.Dir)
			args := []string{"log", "-1", "--format='%H'"}
			cmd := exec.Command("git", args...)
			cmd.Dir = fullpath
			cmd.Stdout = &buf

			err = cmd.Run()
			out := bytes.Trim(buf.Bytes(), "'\n")
			if err != nil {
				log.Printf("# cd %s; %s %s\n", cmd.Dir, "git", strings.Join(args, " "))
				os.Stderr.Write(out)
			} else {
				unit.CommitID = string(out)
			}
			buf.Reset()
		}
	}
}

func assignGodepsCommits(dir string, units []*unit.SourceUnit) {
	fullpath := filepath.Join(dir, "Godeps", "Godeps.json")
	lookup := make(map[string]string)
	deps, err := gog.LoadGodepsFile(fullpath)

	if err != nil {
		log.Println("No Godeps file found.")
		return
	}

	for _, dep := range deps.Deps {
		// @TODO(Abe): Lookup comment for tag
		lookup[dep.ImportPath] = dep.Rev
	}

	for _, unit := range units {
		name, isVendored := vendoredUnitName(unit.Data.(*build.Package))
		if !isVendored {
			name = unit.Name
		}
		if rev, ok := lookup[name]; ok {
			unit.CommitID = rev
		}
	}
}

func assignGovendorCommits(dir string, units []*unit.SourceUnit) {
	fullpath := filepath.Join(dir, "vendor", "vendor.json")
	lookup := make(map[string]string)
	deps, err := gog.LoadGovendorFile(fullpath)

	if err != nil {
		log.Println("No govendor file found.")
		return
	}

	for _, dep := range deps.Package {
		lookup[dep.Path] = dep.Revision
	}

	for _, unit := range units {
		name, isVendored := vendoredUnitName(unit.Data.(*build.Package))
		if !isVendored {
			name = unit.Name
		}
		if rev, ok := lookup[name]; ok {
			unit.CommitID = rev
		}
	}
}

func assignRevisionsToDependencies(units []*unit.SourceUnit) {
	lookup := make(map[string]string)

	for _, unit := range units {
		name, isVendored := vendoredUnitName(unit.Data.(*build.Package))
		if !isVendored {
			name = unit.Name
		}
		lookup[name] = unit.CommitID
	}

	for _, unit := range units {
		for index, dep := range unit.Dependencies {
			newDep := gog.Dep{Name: fmt.Sprintf("%s", dep)}
			if commit, ok := lookup[newDep.Name]; ok {
				newDep.Version = commit
			}
			unit.Dependencies[index] = newDep
		}
	}
}

func scanForPackages(srcdir string, dir string) ([]*build.Package, error) {
	var pkgs []*build.Package

	pkg, err := buildContext.ImportDir(dir, 0)
	if err != nil {
		if _, ok := err.(*build.NoGoError); ok {
			// ignore
		} else {
			log.Printf("Error scanning %s for packages: %v. Ignoring source files in this directory.", dir, err)
		}
	} else {
		// reldir, _ := filepath.Rel(srcdir, dir)
		// pkg.ImportPath = reldir
		pkgs = append(pkgs, pkg)
	}

	infos, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		name := info.Name()
		fullPath := filepath.Join(dir, name)
		if info.IsDir() && ((name[0] != '.' && name[0] != '_' && name != "testdata") || (strings.HasSuffix(filepath.ToSlash(fullPath), "/Godeps/_workspace") && !config.SkipGodeps)) {
			subPkgs, err := scanForPackages(srcdir, fullPath)
			if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, subPkgs...)
		}
	}

	return pkgs, nil
}

// matchPattern(pattern)(name) reports whether
// name matches pattern.  Pattern is a limited glob
// pattern in which '...' means 'any string' and there
// is no other special syntax.
func matchPattern(pattern string) func(name string) bool {
	re := regexp.QuoteMeta(pattern)
	re = strings.Replace(re, `\.\.\.`, `.*`, -1)
	// Special case: foo/... matches foo too.
	if strings.HasSuffix(re, `/.*`) {
		re = re[:len(re)-len(`/.*`)] + `(/.*)?`
	}
	reg := regexp.MustCompile(`^` + re + `$`)
	return func(name string) bool {
		return reg.MatchString(name)
	}
}

// StringSlice attaches the methods of sort.Interface to []string, sorting in decreasing string length
type vendorDirSlice []string

func (p vendorDirSlice) Len() int           { return len(p) }
func (p vendorDirSlice) Less(i, j int) bool { return len(p[i]) >= len(p[j]) }
func (p vendorDirSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

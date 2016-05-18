package gog

import (
  "encoding/json"
  "fmt"
  "os"
)

type GodepDependency struct {
  ImportPath string
  Comment    string `json:",omitempty"` // Description of commit, if present.
  Rev        string // VCS-specific commit ID.

  // used by command save & update
  ws   string // workspace
  root string // import path to repo root
  dir  string // full path to package

  // used by command update
  matched bool // selected for update by command line
}

type Godeps struct {
  ImportPath   string
  GoVersion    string
  GodepVersion string
  Packages     []string `json:",omitempty"` // Arguments to save, if any.
  Deps         []GodepDependency
  isOldFile    bool
}

func LoadGodepsFile(path string) (Godeps, error) {
  var g Godeps
  f, err := os.Open(path)
  if err != nil {
    return g, err
  }
  defer f.Close()
  err = json.NewDecoder(f).Decode(&g)
  if err != nil {
    err = fmt.Errorf("Unable to parse %s: %s", path, err.Error())
  }
  return g, err
}

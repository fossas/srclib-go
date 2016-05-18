package gog

import (
  "encoding/json"
  "fmt"
  "os"
)

type GovendorDependency struct {
  Path string
  Revision string
}

type Govendor struct {
  Package      []GovendorDependency
  rootPath     string
}

func LoadGovendorFile(path string) (Govendor, error) {
  var g Govendor
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

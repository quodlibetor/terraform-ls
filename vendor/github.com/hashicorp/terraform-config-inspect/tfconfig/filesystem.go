package tfconfig

import (
	"io/ioutil"
	"os"
)

// See http://golang.org/s/draft-iofs-design
// TODO: Replace with io/fs.FS when available
type FS interface {
	Open(name string) (File, error)
	ReadFile(name string) ([]byte, error)
	ReadDir(dirname string) ([]os.FileInfo, error)
}

// See http://golang.org/s/draft-iofs-design
// TODO: Replace with io/fs.File when available
type File interface {
	Stat() (os.FileInfo, error)
	Read([]byte) (int, error)
	Close() error
}

type osFs struct{}

func (fs *osFs) Open(name string) (File, error) {
	return os.Open(name)
}

func (fs *osFs) ReadFile(name string) ([]byte, error) {
	return ioutil.ReadFile(name)
}

func (fs *osFs) ReadDir(dirname string) ([]os.FileInfo, error) {
	return ioutil.ReadDir(dirname)
}

func NewOsFs() FS {
	return &osFs{}
}

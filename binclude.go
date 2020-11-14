package binclude

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Debug if set to true files are read via os.Open() and the bincluded files are
// ignored, use when developing.
var Debug = false

// Include this file/ directory (including subdirectories) relative to the package path (noop)
// The path is walked via filepath.Walk and all files found are included
// This function returns the name to make it usable in global variable definitions.
func Include(name string) string { return name }

// IncludeGlob include all files matching the given pattern
// same syntax as filepath.Glob
// This function returns an empty string to make it usable in global variable definitions.
func IncludeGlob(pattern string) string { return "" }

// IncludeFromFile like include but reads paths from a textfile.
// Paths are separated by a newline (noop)
func IncludeFromFile(name string) {}

// FileSystem implements access to a collection of named files.
type FileSystem struct {
	Files
	sync.RWMutex
}

// Files a map from the filepath to the files
type Files map[string]*BincludeFile

// GoString internally used for code generation
func (fs *FileSystem) GoString() string {
	var b strings.Builder
	b.WriteString("&binclude.FileSystem{Files: binclude.Files{\n")

	var paths []string
	for path := range fs.Files {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	for _, path := range paths {
		file := fs.Files[path]
		b.WriteString(fmt.Sprintf("%q: %#v,\n", path, file))

	}

	b.WriteString("}}")

	return b.String()
}

// Open returns a File using the File interface
func (fs *FileSystem) Open(name string) (File, error) {
	if Debug {
		name = filepath.FromSlash(name)

		return os.Open(name)
	}

	name = strings.TrimPrefix(name, "./")
	if f, ok := fs.Files[name]; ok {
		f.reader = bytes.NewReader(f.Content)
		f.path = name
		f.fs = fs
		return f, nil
	}

	return nil, &os.PathError{"open", name, errors.New("File does not exist in binclude map")}
}

// Stat returns a FileInfo describing the named file.
// If there is an error, it will be of type *PathError.
func (fs *FileSystem) Stat(name string) (os.FileInfo, error) {
	f, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return f.Stat()
}

// ReadFile reads the file named by filename and returns the contents.
// A successful call returns err == nil, not err == EOF. Because ReadFile
// reads the whole file, it does not treat an EOF from Read as an error
// to be reported.
func (fs *FileSystem) ReadFile(filename string) ([]byte, error) {
	f, err := fs.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return ioutil.ReadAll(f)
}

// ReadDir reads the directory named by dirname and returns
// a list of directory entries sorted by filename.
func (fs *FileSystem) ReadDir(dirname string) ([]os.FileInfo, error) {
	f, err := fs.Open(dirname)
	if err != nil {
		return nil, err
	}
	list, _ := f.Readdir(-1)
	f.Close()
	sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
	return list, nil
}

// CopyFile copies a specific file from a binclude FileSystem to the hosts FileSystem.
// Permissions are copied from the included file.
func (fs *FileSystem) CopyFile(bincludePath, hostPath string) error {
	src, err := fs.Open(bincludePath)
	if err != nil {
		return err
	}
	defer src.Close()

	info, _ := src.Stat()

	dst, err := os.OpenFile(hostPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	if err != nil {
		return err
	}

	info, err = os.Stat(hostPath)
	if err != nil {
		return err
	}

	return nil
}

// Compression the compression algorithm to use
type Compression int

// GoString internally used for code generation
func (c Compression) GoString() string {
	switch c {
	case None:
		return "binclude.None"
	case Gzip:
		return "binclude.Gzip"
	}

	panic(fmt.Sprint(int(c), "is not a valid compression algorithm"))
}

const (
	// None dont compress
	None Compression = iota
	// Gzip use gzip compression
	Gzip
)

// Decompress turns a FileSystem with compressed files into a filesystem without compressed files
func (fs *FileSystem) Decompress() (err error) {
	for path, file := range fs.Files {
		if file.Compression == None {
			continue
		}

		f, _ := fs.Open(path) // open cannot error when using a path we got from the fs
		defer f.Close()

		var compReader io.Reader
		if file.Compression == Gzip {
			compReader, err = gzip.NewReader(f)
			if err != nil {
				return fmt.Errorf("Gzip err: %v", err)
			}
		}

		content, err := ioutil.ReadAll(compReader)
		if err != nil {
			return fmt.Errorf("Reader err: %v", err)
		}
		f.Close()

		fs.Files[path].Content = content
	}

	return nil

}

// Compress turns a FileSystem without compressed files into a filesystem with compressed files
func (fs *FileSystem) Compress(algo Compression) error {
	if algo == None {
		return nil
	}
	for _, file := range fs.Files {
		if file.Mode.IsDir() || !shouldCompress(file.Filename) {
			continue
		}
		var b bytes.Buffer

		var writer io.WriteCloser
		if algo == Gzip {
			writer = gzip.NewWriter(&b)
		}

		_, err := writer.Write(file.Content)
		writer.Close()
		if err != nil {
			return err
		}

		file.Compression = algo
		file.Content = b.Bytes()
	}

	return nil
}

// compressExcl exclude certain files from compression which don't compress well
// inspired by https://github.com/gin-contrib/gzip/blob/master/options.go
var compressExcl = []string{".jpg", ".jpeg", ".gz", ".png", ".gif", ".zip"}

// shouldCompress says whether a file should be compressed based on its mimetype
func shouldCompress(name string) bool {
	for _, excl := range compressExcl {
		if strings.HasSuffix(name, excl) {
			return false
		}
	}
	return true
}

// File same as http.File
type File interface {
	io.Closer
	io.Reader
	io.Seeker
	Readdir(count int) ([]os.FileInfo, error)
	Stat() (os.FileInfo, error)
}

// BincludeFile implements the io.Reader, io.Seeker, io.Closer and http.File interfaces
type BincludeFile struct {
	Filename string
	Mode     os.FileMode
	ModTime  time.Time
	Content  []byte
	Compression
	reader io.ReadSeeker
	path   string
	fs     *FileSystem
}

// check that the File interface is implemented
var _ File = new(BincludeFile)

// Read implements the io.Reader interface.
func (f *BincludeFile) Read(p []byte) (n int, err error) {
	return f.reader.Read(p)
}

// Name returns the name of the file as presented to Open.
func (f *BincludeFile) Name() string {
	return f.path
}

// Close closes the File, rendering it unusable for I/O.
func (f *BincludeFile) Close() error {
	f.reader = nil
	return nil
}

// Size returns the original length of the underlying byte slice.
// Size is the number of bytes available for reading via ReadAt.
// The returned value is always the same and is not affected by calls
// to any other method.
func (f *BincludeFile) Size() int64 {
	return int64(len(f.Content))
}

// Readdir reads the contents of the directory associated with file and
// returns a slice of up to n FileInfo values, as would be returned
// by Lstat, in directory order. Subsequent calls on the same file will yield
// further FileInfos.
func (f *BincludeFile) Readdir(count int) (infos []os.FileInfo, err error) {
	fileDir := f.Name()
	if !f.Mode.IsDir() {
		fileDir = filepath.Dir(f.path)
	}

	for path, file := range *&f.fs.Files {
		if filepath.Dir(path) != fileDir {
			continue
		}

		info, _ := file.Stat()

		infos = append(infos, info)
	}

	return infos, nil
}

// Stat returns the FileInfo structure describing file.
// Error is always nil
func (f *BincludeFile) Stat() (os.FileInfo, error) {
	return &FileInfo{
		name:    f.Filename,
		mode:    f.Mode,
		size:    f.Size(),
		modtime: f.ModTime,
	}, nil
}

// Seek implements the io.Seeker interface.
func (f *BincludeFile) Seek(offset int64, whence int) (int64, error) {
	return f.reader.Seek(offset, whence)
}

func (f *BincludeFile) timeString() string {
	return fmt.Sprint("time.Unix(", f.ModTime.Unix(), ", ", f.ModTime.UnixNano(), ")")
}

// GoString internally used for code generation
func (f *BincludeFile) GoString() string {
	return fmt.Sprintf(`{
	Filename: %q, Mode: %O, ModTime: %s, Compression: %#v, 
Content: []byte(%q),
}`,
		f.Filename, f.Mode, f.timeString(), f.Compression, f.Content)
}

// FileInfo implements the os.FileInfo interface.
type FileInfo struct {
	name    string
	mode    os.FileMode
	modtime time.Time
	size    int64
}

// check that the os.FileInfo interface is implemented
var _ os.FileInfo = new(FileInfo)

// Name returns the base name of the file
func (info *FileInfo) Name() string {
	return info.name
}

// Size returns the length in bytes
func (info *FileInfo) Size() int64 {
	return info.size
}

// Mode returns the file mode bits
func (info *FileInfo) Mode() os.FileMode {
	return info.mode
}

// ModTime returns the modification time (returns current time)
func (info *FileInfo) ModTime() time.Time {
	return info.modtime
}

// IsDir abbreviation for Mode().IsDir()
func (info *FileInfo) IsDir() bool {
	return info.Mode().IsDir()
}

// Sys underlying data source (returns nil)
func (info *FileInfo) Sys() interface{} {
	return nil
}

package ward

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// walkRoot visits raw relative names beneath root without io/fs path
// validation or absolute-path reconstruction. descend controls directory
// recursion; the root itself is reported as ".".
func walkRoot(root *os.Root, visit func(string, fs.DirEntry) (bool, error)) error {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if _, err := visit(".", fs.FileInfoToDirEntry(rootInfo)); err != nil {
		return err
	}
	var walk func(string) error
	walk = func(dir string) error {
		handle, err := root.Open(filepath.FromSlash(dir))
		if err != nil {
			return err
		}
		children, readErr := handle.ReadDir(-1)
		closeErr := handle.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			rel := child.Name()
			if dir != "." {
				rel = dir + "/" + child.Name()
			}
			descend, err := visit(rel, child)
			if err != nil {
				return err
			}
			if child.IsDir() && descend {
				if err := walk(rel); err != nil {
					return fmt.Errorf("walk rooted directory %q: %w", rel, err)
				}
			}
		}
		return nil
	}
	return walk(".")
}

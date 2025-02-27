package pail

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/mongodb/grip"
	"github.com/pkg/errors"
)

func walkLocalTree(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	err := filepath.Walk(prefix, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		if ctx.Err() != nil {
			return errors.New("operation canceled")
		}

		rel, err := filepath.Rel(prefix, path)
		if err != nil {
			return errors.Wrap(err, "getting relative path")
		}

		if info.Mode()&os.ModeSymlink != 0 {
			symPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				return errors.Wrap(err, "getting symlink path")
			}
			symTree, err := walkLocalTree(ctx, symPath)
			if err != nil {
				return errors.Wrap(err, "getting symlink tree")
			}
			for i := range symTree {
				symTree[i] = filepath.Join(rel, symTree[i])
			}
			out = append(out, symTree...)

			return nil
		}

		if info.IsDir() {
			return nil
		}

		out = append(out, rel)
		return nil
	})

	if err != nil {
		return nil, errors.Wrap(err, "finding files")
	}
	if ctx.Err() != nil {
		return nil, errors.New("operation canceled")
	}

	return out, nil
}

func removePrefix(ctx context.Context, prefix string, b Bucket) error {
	iter, err := b.List(ctx, prefix)
	if err != nil {
		return errors.Wrapf(err, "listing objects with prefix '%s'", prefix)
	}

	keys := []string{}
	for iter.Next(ctx) {
		keys = append(keys, iter.Item().Name())
	}
	return errors.Wrapf(b.RemoveMany(ctx, keys...), "deleting objects with prefix '%s'", prefix)
}

func removeMatching(ctx context.Context, expression string, b Bucket) error {
	regex, err := regexp.Compile(expression)
	if err != nil {
		return errors.Wrapf(err, "invalid regular expression '%s'", expression)
	}
	iter, err := b.List(ctx, "")
	if err != nil {
		return errors.Wrapf(err, "listing objects matching '%s'", expression)
	}

	keys := []string{}
	for iter.Next(ctx) {
		key := iter.Item().Name()
		if regex.MatchString(key) {
			keys = append(keys, key)
		}
	}
	return errors.Wrapf(b.RemoveMany(ctx, keys...), "deleting objects matching '%s'", expression)
}

func consistentJoin(prefix, key string) string {
	if prefix != "" {
		return prefix + "/" + key
	}
	return key
}

func deleteOnPush(ctx context.Context, sourceFiles []string, remote string, bucket Bucket) error {
	sourceFilesMap := map[string]bool{}
	for _, fn := range sourceFiles {
		sourceFilesMap[fn] = true
	}

	iter, err := bucket.List(ctx, remote)
	if err != nil {
		return err
	}

	toDelete := []string{}
	for iter.Next(ctx) {
		fn := strings.TrimPrefix(iter.Item().Name(), remote)
		fn = strings.TrimPrefix(fn, "/")
		fn = strings.TrimPrefix(fn, "\\") // cause windows...

		if !sourceFilesMap[fn] {
			toDelete = append(toDelete, iter.Item().Name())
		}
	}

	return bucket.RemoveMany(ctx, toDelete...)
}

func deleteOnPull(ctx context.Context, sourceFiles []string, local string) error {
	sourceFilesMap := map[string]bool{}
	for _, fn := range sourceFiles {
		sourceFilesMap[fn] = true
	}

	destinationFiles, err := walkLocalTree(ctx, local)
	if err != nil {
		return errors.WithStack(err)
	}

	catcher := grip.NewBasicCatcher()
	for _, fn := range destinationFiles {
		if !sourceFilesMap[fn] {
			catcher.Add(os.RemoveAll(filepath.Join(local, fn)))
		}
	}

	return catcher.Resolve()
}

// The archive/unarchive functions below are modified versions of the same
// functions from github.com/mholt/archiver.

func tarFile(tarWriter *tar.Writer, dir, relPath string) error {
	var absPath string
	if filepath.IsAbs(relPath) {
		if !strings.HasPrefix(relPath, dir) {
			return errors.Errorf("cannot specify absolute path to file that is not within directory '%s'", dir)
		}
		absPath = relPath
		var err error
		relPath, err = filepath.Rel(dir, relPath)
		if err != nil {
			return errors.Wrap(err, "getting relative path")
		}
	} else {
		absPath = filepath.Join(dir, relPath)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return errors.Wrapf(err, "getting stat info for path '%s'", absPath)
	}

	var file *os.File
	if info.Mode().IsRegular() {
		file, err = os.Open(absPath)
		if err != nil {
			return errors.Wrap(err, "opening file")
		}
		defer file.Close()
	}
	if err := addToTar(tarWriter, info, file, absPath, relPath); err != nil {
		return errors.Wrap(err, "adding file to archive")
	}

	return nil
}

func addToTar(tarWriter *tar.Writer, info os.FileInfo, content io.Reader, absPath, relPath string) error {
	header, err := tar.FileInfoHeader(info, absPath)
	if err != nil {
		return errors.Wrap(err, "creating header")
	}

	header.Name = filepath.ToSlash(relPath)

	if info.IsDir() {
		header.Name += "/"
	}

	if err := tarWriter.WriteHeader(header); err != nil {
		return errors.Wrap(err, "writing header")
	}

	if header.Typeflag == tar.TypeReg {
		if _, err := io.CopyN(tarWriter, content, info.Size()); err != nil && err != io.EOF {
			return errors.Wrap(err, "archiving contents")
		}
	}
	return nil
}

func untar(tarReader *tar.Reader, destination string, exclude *regexp.Regexp) error {
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if exclude != nil && exclude.MatchString(header.Name) {
			continue
		}

		if err := untarFile(tarReader, header, destination); err != nil {
			return errors.Wrap(err, header.Name)
		}
	}
}

// untarFile untars a single file from the tar reader with header header into destination.
func untarFile(tarReader *tar.Reader, header *tar.Header, destination string) error {
	err := sanitizeExtractPath(header.Name, destination)
	if err != nil {
		return err
	}

	destpath := filepath.Join(destination, header.Name)

	switch header.Typeflag {
	case tar.TypeDir:
		return mkdir(destpath)
	case tar.TypeReg, tar.TypeRegA, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return writeFile(destpath, tarReader, header.FileInfo().Mode())
	case tar.TypeSymlink:
		return writeSymlink(destpath, header.Linkname)
	case tar.TypeLink:
		return writeHardLink(destpath, filepath.Join(destination, header.Linkname))
	case tar.TypeXGlobalHeader:
		// ignore the pax global header from git generated tarballs
		return nil
	default:
		return errors.Errorf("unknown type flag %c", header.Typeflag)
	}
}

func sanitizeExtractPath(filePath string, destination string) error {
	// to avoid zip slip (writing outside of the destination), we resolve
	// the target path, and make sure it's nested in the intended
	// destination, or bail otherwise.
	destpath := filepath.Join(destination, filePath)
	if !strings.HasPrefix(destpath, filepath.Clean(destination)) {
		return errors.Errorf("illegal file path '%s'", filePath)
	}
	return nil
}

func mkdir(dirPath string) error {
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return errors.Wrapf(err, "making directory '%s'", dirPath)
	}
	return nil
}

func writeFile(path string, content io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errors.Wrapf(err, "making parent directories for file '%s'", path)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0700)
	if err != nil {
		return err
	}
	defer file.Close()

	if err = file.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		return err
	}

	if _, err = io.Copy(file, content); err != nil {
		return err
	}
	return nil
}

func writeSymlink(path string, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errors.Wrapf(err, "making parent directories for file '%s'", path)
	}

	if err := os.Symlink(target, path); err != nil {
		return errors.Wrapf(err, "making symbolic link for path '%s'", path)
	}

	return nil
}

func writeHardLink(path string, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errors.Wrapf(err, "making parent directories for file '%s'", path)
	}

	if err := os.Link(target, path); err != nil {
		return errors.Wrapf(err, "making hard link for '%s'", path)
	}

	return nil
}

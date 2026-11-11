package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

func PutFile(ctx context.Context, store ports.ArtifactStore, scope ports.SecurityScope, path string) (domain.CASRef, error) {
	file, err := os.Open(path)
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("open artifact %s: %w", path, err)
	}
	defer file.Close()
	return store.Put(ctx, scope, file)
}

func PutJSON(ctx context.Context, store ports.ArtifactStore, scope ports.SecurityScope, value any) (domain.CASRef, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("marshal artifact: %w", err)
	}
	return store.Put(ctx, scope, strings.NewReader(string(raw)))
}

func ReadJSON(ctx context.Context, store ports.ArtifactStore, scope ports.SecurityScope, ref domain.CASRef, value any) error {
	reader, err := store.Open(ctx, scope, ref)
	if err != nil {
		return err
	}
	defer reader.Close()
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
	return decoder.Decode(value)
}

func PutDirTarGz(ctx context.Context, store ports.ArtifactStore, scope ports.SecurityScope, root string) (domain.CASRef, error) {
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(writeTarGz(root, writer))
	}()
	return store.Put(ctx, scope, reader)
}

func writeTarGz(root string, output io.Writer) error {
	gzipWriter, err := gzip.NewWriterLevel(output, gzip.BestSpeed)
	if err != nil {
		return err
	}
	defer gzipWriter.Close()
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, relative)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, relative := range paths {
		full := filepath.Join(root, relative)
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		header := &tar.Header{
			Name:    filepath.ToSlash(relative),
			Mode:    int64(info.Mode().Perm()),
			ModTime: time.Unix(0, 0),
			Uid:     0,
			Gid:     0,
			Uname:   "",
			Gname:   "",
		}
		if info.IsDir() {
			header.Typeflag = tar.TypeDir
			header.Name += "/"
			if err := tarWriter.WriteHeader(header); err != nil {
				return err
			}
			continue
		}
		header.Typeflag = tar.TypeReg
		header.Size = info.Size()
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		file, err := os.Open(full)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tarWriter, file); err != nil {
			file.Close()
			return err
		}
		file.Close()
	}
	return nil
}

//go:build ignore

// Command package writes one platform's release archive: a zip holding the
// shared library and its licence notices at its root, beside the sha256sum
// line for that zip.
//
// The plugin store reads the library out of the zip root by basename, rejects
// an archive carrying a second dynamic library, and ignores every other file,
// so the notices ride along without reaching the plugin directory. The
// c-shared build emits a C header next to the library; naming the library
// explicitly is what keeps the header out.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// notices are the licence files every archive carries, read from the
// repository root. The library statically links code under each of them.
var notices = []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES"}

func main() {
	lib := flag.String("lib", "", "path to the built shared library")
	out := flag.String("out", "", "path of the zip to write")
	flag.Parse()
	if *lib == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: package -lib <library> -out <zip>")
		os.Exit(2)
	}
	if err := write(*lib, *out); err != nil {
		fmt.Fprintln(os.Stderr, "package:", err)
		os.Exit(1)
	}
}

// write builds the archive and its checksum file. The checksum covers the zip
// as a whole, which is what the store verifies before it opens the archive.
func write(lib, out string) error {
	body, errRead := os.ReadFile(lib)
	if errRead != nil {
		return errRead
	}
	if errDir := os.MkdirAll(filepath.Dir(out), 0o755); errDir != nil {
		return errDir
	}

	archive, errCreate := os.Create(out)
	if errCreate != nil {
		return errCreate
	}
	writer := zip.NewWriter(archive)
	// The executable bit rides in the zip entry: the store installs the file
	// as it finds it and never re-applies a mode of its own.
	header := &zip.FileHeader{Name: filepath.Base(lib), Method: zip.Deflate}
	header.SetMode(0o755)
	entry, errEntry := writer.CreateHeader(header)
	if errEntry != nil {
		archive.Close()
		return errEntry
	}
	if _, errWrite := entry.Write(body); errWrite != nil {
		archive.Close()
		return errWrite
	}
	for _, name := range notices {
		if errNotice := addNotice(writer, name); errNotice != nil {
			archive.Close()
			return errNotice
		}
	}
	if errClose := writer.Close(); errClose != nil {
		archive.Close()
		return errClose
	}
	if errClose := archive.Close(); errClose != nil {
		return errClose
	}

	packed, errPacked := os.ReadFile(out)
	if errPacked != nil {
		return errPacked
	}
	sum := sha256.Sum256(packed)
	// Two spaces between digest and name: sha256sum's own text-mode format,
	// which is what the store's checksum parser reads.
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), filepath.Base(out))
	if errSum := os.WriteFile(out+".sha256", []byte(line), 0o644); errSum != nil {
		return errSum
	}
	fmt.Printf("packaged %s\n", out)
	return nil
}

// addNotice copies one licence file from the working directory into the
// archive root.
func addNotice(writer *zip.Writer, name string) error {
	body, errRead := os.ReadFile(name)
	if errRead != nil {
		return errRead
	}
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o644)
	entry, errEntry := writer.CreateHeader(header)
	if errEntry != nil {
		return errEntry
	}
	_, errWrite := entry.Write(body)
	return errWrite
}

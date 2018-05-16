package packager

//go:generate go-bindata -pkg $GOPACKAGE -prefix scaffold scaffold/...

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/cloudfoundry/libbuildpack"
	"github.com/cloudfoundry/libbuildpack/ansicleaner"
)

var CacheDir = filepath.Join(os.Getenv("HOME"), ".buildpack-packager", "cache")
var Stdout, Stderr io.Writer = os.Stdout, os.Stderr

func CompileExtensionPackage(bpDir, version string, cached bool) (string, error) {
	bpDir, err := filepath.Abs(bpDir)
	if err != nil {
		return "", err
	}
	dir, err := copyDirectory(bpDir)
	if err != nil {
		return "", err
	}

	err = ioutil.WriteFile(filepath.Join(dir, "VERSION"), []byte(version), 0644)
	if err != nil {
		return "", err
	}

	isCached := "--uncached"
	if cached {
		isCached = "--cached"
	}
	cmd := exec.Command("bundle", "exec", "buildpack-packager", isCached)
	cmd.Stdout = Stdout
	cmd.Stderr = Stderr
	cmd.Env = append(os.Environ(), "BUNDLE_GEMFILE=cf.Gemfile")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return "", err
	}

	var manifest struct {
		Language string `yaml:"language"`
	}

	if err := libbuildpack.NewYAML().Load(filepath.Join(bpDir, "manifest.yml"), &manifest); err != nil {
		return "", err
	}

	zipFile := fmt.Sprintf("%s_buildpack-v%s.zip", manifest.Language, version)
	if cached {
		zipFile = fmt.Sprintf("%s_buildpack-cached-v%s.zip", manifest.Language, version)
	}
	if err := libbuildpack.CopyFile(filepath.Join(dir, zipFile), filepath.Join(bpDir, zipFile)); err != nil {
		return "", err
	}

	return filepath.Join(dir, zipFile), nil
}

func hasStack(manifest *libbuildpack.Manifest, stack string) bool {
	for _, e := range manifest.ManifestEntries {
		for _, s := range e.CFStacks {
			if s == stack {
				return true
			}
		}
	}
	return false
}

func validateStack(stack, bpDir string) error {
	if stack == "" {
		return nil
	}

	buffer := new(bytes.Buffer)
	logger := libbuildpack.NewLogger(ansicleaner.New(buffer))
	manifest, err := libbuildpack.NewManifest(bpDir, logger, time.Now())
	if err != nil {
		return err
	}

	if !hasStack(manifest, stack) {
		return fmt.Errorf("Stack `%s` not found in manifest", stack)
	}

	//TODO: PLZ NOT use the env for this???
	if err = os.Setenv("CF_STACK", stack); err != nil {
		return err
	}
	defer os.Unsetenv("CF_STACK")

	for _, d := range manifest.DefaultVersions {
		if _, err := manifest.DefaultVersion(d.Name); err != nil {
			return fmt.Errorf("No matching default dependency `%s` for stack `%s`", d.Name, stack)
		}
	}

	return nil
}

func updateDependencyMap(dependencyMap interface{}, file File) error {
	dep, ok := dependencyMap.(map[interface{}]interface{})
	if !ok {
		return fmt.Errorf("Could not cast deps[idx] to map[interface{}]interface{}")
	}
	dep["file"] = file.Name
	return nil
}

func downloadDependency(dependency Dependency, cacheDir string) (File, error) {
	file := filepath.Join("dependencies", fmt.Sprintf("%x", md5.Sum([]byte(dependency.URI))), filepath.Base(dependency.URI))
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		log.Fatalf("error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, file)); err != nil {
		if err := downloadFromURI(dependency.URI, filepath.Join(cacheDir, file)); err != nil {
			return File{}, err
		}
	}

	if err := checkSha256(filepath.Join(cacheDir, file), dependency.SHA256); err != nil {
		return File{}, err
	}

	return File{file, filepath.Join(cacheDir, file)}, nil
}

func Package(bpDir, cacheDir, version, stack string, cached bool) (string, error) {
	bpDir, err := filepath.Abs(bpDir)
	if err != nil {
		return "", err
	}
	err = validateStack(stack, bpDir)
	if err != nil {
		return "", err
	}
	dir, err := copyDirectory(bpDir)
	if err != nil {
		return "", err
	}

	err = ioutil.WriteFile(filepath.Join(dir, "VERSION"), []byte(version), 0644)
	if err != nil {
		return "", err
	}

	manifest, err := readManifest(dir)
	if err != nil {
		return "", err
	}

	if manifest.PrePackage != "" {
		cmd := exec.Command(manifest.PrePackage)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintln(Stdout, string(out))
			return "", err
		}
	}

	files := []File{}
	for _, name := range manifest.IncludeFiles {
		files = append(files, File{name, filepath.Join(dir, name)})
	}

	var m map[string]interface{}
	if err := libbuildpack.NewYAML().Load(filepath.Join(dir, "manifest.yml"), &m); err != nil {
		return "", err
	}

	if stack != "" {
		m["stack"] = stack
	}

	deps, ok := m["dependencies"].([]interface{})
	if !ok {
		return "", fmt.Errorf("Could not cast dependencies to []interface{}")
	}
	dependenciesForStack := []interface{}{}
	for idx, d := range manifest.Dependencies {
		for _, s := range d.Stacks {
			if stack == "" || s == stack {
				dependencyMap := deps[idx]
				if cached {
					if file, err := downloadDependency(d, cacheDir); err != nil {
						return "", err
					} else {
						updateDependencyMap(dependencyMap, file)
						files = append(files, file)
					}
				}

				dependenciesForStack = append(dependenciesForStack, dependencyMap)
				break
			}
		}
	}
	m["dependencies"] = dependenciesForStack

	if err := libbuildpack.NewYAML().Write(filepath.Join(dir, "manifest.yml"), m); err != nil {
		return "", err
	}

	zipFile := fmt.Sprintf("%s_buildpack-v%s.zip", manifest.Language, version)
	if cached {
		zipFile = fmt.Sprintf("%s_buildpack-cached-v%s.zip", manifest.Language, version)
	}
	zipFile = filepath.Join(bpDir, zipFile)

	ZipFiles(zipFile, files)

	return zipFile, err
}

func downloadFromURI(uri, fileName string) error {
	err := os.MkdirAll(filepath.Dir(fileName), 0755)
	if err != nil {
		return err
	}

	output, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer output.Close()

	u, err := url.Parse(uri)
	if err != nil {
		return err
	}

	var source io.ReadCloser

	if u.Scheme == "file" {
		source, err = os.Open(u.Path)
		if err != nil {
			return err
		}
		defer source.Close()
	} else {
		response, err := http.Get(uri)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		source = response.Body

		if response.StatusCode < 200 || response.StatusCode > 299 {
			return fmt.Errorf("could not download: %d", response.StatusCode)
		}
	}

	_, err = io.Copy(output, source)

	return err
}

func checkSha256(filePath, expectedSha256 string) error {
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		return err
	}

	sum := sha256.Sum256(content)

	actualSha256 := hex.EncodeToString(sum[:])

	if actualSha256 != expectedSha256 {
		return fmt.Errorf("dependency sha256 mismatch: expected sha256 %s, actual sha256 %s", expectedSha256, actualSha256)
	}
	return nil
}

func ZipFiles(filename string, files []File) error {
	newfile, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer newfile.Close()

	zipWriter := zip.NewWriter(newfile)
	defer zipWriter.Close()

	// Add files to zip
	for _, file := range files {

		zipfile, err := os.Open(file.Path)
		if err != nil {
			return err
		}
		defer zipfile.Close()

		// Get the file information
		info, err := zipfile.Stat()
		if err != nil {
			return err
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		// Change to deflate to gain better compression
		// see http://golang.org/pkg/archive/zip/#pkg-constants
		header.Method = zip.Deflate
		header.Name = file.Name

		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = io.Copy(writer, zipfile)
		if err != nil {
			return err
		}
	}
	return nil
}

func copyDirectory(srcDir string) (string, error) {
	destDir, err := ioutil.TempDir("", "buildpack-packager")
	if err != nil {
		return "", err
	}
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		path, err = filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		if path == ".git" || path == "tests" {
			return filepath.SkipDir
		}

		dest := filepath.Join(destDir, path)
		if m := info.Mode(); m&os.ModeSymlink != 0 {
			srcPath := filepath.Join(srcDir, path)
			target, err := os.Readlink(srcPath)
			if err != nil {
				return fmt.Errorf("Error while reading symlink '%s': %v", srcPath, err)
			}
			if err := os.Symlink(target, dest); err != nil {
				return fmt.Errorf("Error while creating '%s' as symlink to '%s': %v", dest, target, err)
			}
		} else if info.IsDir() {
			err = os.MkdirAll(dest, info.Mode())
			if err != nil {
				return err
			}
		} else {
			src, err := os.Open(filepath.Join(srcDir, path))
			if err != nil {
				return err
			}
			defer src.Close()

			err = os.MkdirAll(filepath.Dir(dest), 0755)
			if err != nil {
				return err
			}

			fh, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode())
			if err != nil {
				return err
			}

			_, err = io.Copy(fh, src)
			fh.Close()
			return err
		}
		return nil
	})
	return destDir, err
}

package environment

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// checkComposeFiles bounds the reads that disappear from Compose's model.
// Keep local composition rather than banning it (decision 2026-11-19-compose-local-files).
// src is an archive without symlinks, owned by the runner for this turn.
func checkComposeFiles(src, file string) error {
	type location struct{ file, dir string }
	seen := map[location]bool{}
	active := map[string]bool{}
	var visit func(string, string) error
	visit = func(file, dir string) error {
		loc := location{file, dir}
		if seen[loc] {
			return nil
		}
		if active[file] {
			return fmt.Errorf("recursive compose file reference changes its project directory: %s", file)
		}
		active[file] = true
		defer delete(active, file)
		seen[loc] = true
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var model map[string]any
		dec := yaml.NewDecoder(bytes.NewReader(b))
		if err := dec.Decode(&model); err != nil {
			return fmt.Errorf("compose file %s: %w", file, err)
		}
		// Compose stringifies mapping keys; numeric service names must not hide
		// neighbouring services from the preflight's string-keyed lookups.
		if _, err := composeStringKeys(model); err != nil {
			return err
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return fmt.Errorf("compose file %s: multiple YAML documents are not allowed", file)
		}
		if includes, ok := model["include"]; ok && includes != nil {
			list, ok := includes.([]any)
			if !ok {
				return fmt.Errorf("compose include must be a list")
			}
			for _, entry := range list {
				var paths, envFiles []string
				projectDir := ""
				switch entry := entry.(type) {
				case string:
					paths = []string{entry}
				case map[string]any:
					paths, err = composeFilePaths(entry["path"])
					if err != nil {
						return err
					}
					envFiles, err = composeFilePaths(entry["env_file"])
					if err != nil {
						return err
					}
					if v, ok := entry["project_directory"]; ok {
						var good bool
						projectDir, good = v.(string)
						if !good {
							return fmt.Errorf("include project_directory must be a literal path")
						}
					}
				default:
					return fmt.Errorf("compose include must name a file")
				}
				if len(paths) == 0 {
					return fmt.Errorf("compose include must name a file")
				}
				for i, path := range paths {
					paths[i], err = composeLocalPath(src, dir, path)
					if err != nil {
						return err
					}
				}
				for _, path := range envFiles {
					if _, err := composeLocalPath(src, dir, path); err != nil {
						return err
					}
				}
				if projectDir == "" {
					projectDir = filepath.Dir(paths[0])
				} else {
					projectDir, err = composeLocalPath(src, dir, projectDir)
					if err != nil {
						return err
					}
				}
				for _, path := range paths {
					if err := visit(path, projectDir); err != nil {
						return err
					}
				}
			}
		}
		services, _ := model["services"].(map[string]any)
		for _, value := range services {
			service, _ := value.(map[string]any)
			// Like include, label_file is consumed before cfg.check can see it.
			if _, ok := service["label_file"]; ok {
				return fmt.Errorf("service sets label_file, which the runner does not allow")
			}
			if extends, ok := service["extends"].(map[string]any); ok {
				if value, ok := extends["file"]; ok {
					path, ok := value.(string)
					if !ok {
						return fmt.Errorf("extends.file must be a literal path")
					}
					path, err = composeLocalPath(src, dir, path)
					if err != nil {
						return err
					}
					if err := visit(path, filepath.Dir(path)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	path, err := composeLocalPath(src, src, file)
	if err != nil {
		return err
	}
	return visit(path, filepath.Dir(path))
}

func composeFilePaths(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	if path, ok := value.(string); ok {
		return []string{path}, nil
	}
	if list, ok := value.([]any); ok {
		paths := make([]string, 0, len(list))
		for _, v := range list {
			path, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("compose file reference must be a literal path")
			}
			paths = append(paths, path)
		}
		return paths, nil
	}
	return nil, fmt.Errorf("compose file reference must be a literal path or list")
}

func composeLocalPath(src, dir, path string) (string, error) {
	if path == "" || strings.ContainsAny(strings.TrimPrefix(path, filepath.VolumeName(path)), "$~:") {
		return "", fmt.Errorf("compose file reference %q must be a literal local path", path)
	}
	path = filepath.FromSlash(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	rel, err := filepath.Rel(src, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("compose file reference %q is outside the repository", path)
	}
	return filepath.Clean(path), nil
}

func composeStringKeys(value any) (any, error) {
	if mixed, ok := value.(map[any]any); ok {
		mapping := make(map[string]any, len(mixed))
		for key, v := range mixed {
			k := fmt.Sprint(key)
			if _, exists := mapping[k]; exists {
				return nil, fmt.Errorf("compose mapping keys collide after conversion to strings")
			}
			mapping[k] = v
		}
		value = mapping
	}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			normalized, err := composeStringKeys(child)
			if err != nil {
				return nil, err
			}
			v[key] = normalized
		}
	case []any:
		for i, child := range v {
			normalized, err := composeStringKeys(child)
			if err != nil {
				return nil, err
			}
			v[i] = normalized
		}
	}
	return value, nil
}

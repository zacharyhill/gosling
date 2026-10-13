package lsp

import (
	"encoding/json"
	"path/filepath"
)

// fileOperationParams holds the files of didRenameFiles and didDeleteFiles.
type fileOperationParams struct {
	Files []struct {
		URI    string `json:"uri"`
		OldURI string `json:"oldUri"`
		NewURI string `json:"newUri"`
	} `json:"files"`
}

// handleFileOperation updates the directories of renamed and deleted .gol
// files.
func (server *server) handleFileOperation(message *rpcMessage) error {
	var params fileOperationParams
	if err := json.Unmarshal(message.Params, &params); err != nil {
		server.logger.Printf("bad %s params: %v", message.Method, err)
		return nil
	}
	clear(server.external)
	for _, dir := range changedDirs(params) {
		if err := server.refreshDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// changedDirs returns the directories of the .gol files in params, each one
// time.
func changedDirs(params fileOperationParams) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, file := range params.Files {
		for _, uri := range []string{file.URI, file.OldURI, file.NewURI} {
			golPath, isGol := golPathOf(uri)
			if !isGol {
				continue
			}
			dir := filepath.Dir(golPath)
			if seen[dir] {
				continue
			}
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// refreshDir writes the generated files of dir and transpiles its open
// package again. The write comes first. It deletes the output of an old
// name, and the new package must not include that output.
func (server *server) refreshDir(dir string) error {
	if server.generateOnSave {
		// gosling deletes orphans before it transpiles. So a source error
		// stops only the new output, and the diagnostics show the error.
		if err := server.generator.Write(dir); err != nil {
			server.logger.Printf("write %s: %v", dir, err)
		}
	}
	packageState, ok := server.packages[dir]
	if !ok {
		return nil
	}
	return server.regenerate(packageState)
}

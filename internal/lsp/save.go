package lsp

// messageTypeError is the LSP MessageType for an error in window/showMessage.
const messageTypeError = 1

// writeOnSave writes the generated files of the package of golPath. A package
// with gosling errors keeps its old files, because its diagnostics already
// show the errors.
func (server *server) writeOnSave(golPath string) error {
	if !server.generateOnSave {
		return nil
	}
	packageState, err := server.packageFor(golPath)
	if err != nil || packageState == nil || len(packageState.problems) > 0 {
		return err
	}
	if err := server.generator.Write(packageState.dir); err != nil {
		server.logger.Printf("write %s: %v", packageState.dir, err)
		return server.notifyClient("window/showMessage", map[string]any{
			"type":    messageTypeError,
			"message": "gosling did not write the generated files: " + err.Error(),
		})
	}
	return nil
}

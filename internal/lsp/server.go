package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// regenerateDelay is how long the server waits after the last edit before
// it transpiles the package again.
const regenerateDelay = 200 * time.Millisecond

// Options configures Serve.
type Options struct {
	Generator Generator
	// GoplsPath is the gopls command to run.
	GoplsPath string
	// GenerateOnSave writes the generated .go files of a package when the
	// editor saves one of its .gol files.
	GenerateOnSave bool
	Logger         *log.Logger
}

// Serve runs the language server on the client streams until the client
// sends exit or closes its input. It runs gopls as a child process.
func Serve(ctx context.Context, clientIn io.Reader, clientOut io.Writer, options Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(ctx, options.GoplsPath)
	command.Stderr = options.Logger.Writer()
	goplsIn, err := command.StdinPipe()
	if err != nil {
		return err
	}
	goplsOut, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start gopls: %w", err)
	}
	server := newServer(options, clientOut, goplsIn)
	goplsMessages := server.readMessages(goplsOut, "gopls")
	runErr := server.run(ctx, server.readMessages(clientIn, "client"), goplsMessages)
	if err := goplsIn.Close(); err != nil {
		options.Logger.Printf("close gopls input: %v", err)
	}
	if !server.exiting {
		cancel()
	}
	for range goplsMessages {
		// Drain the reader so it stops before Wait closes the pipe.
	}
	if err := command.Wait(); err != nil && server.exiting && runErr == nil {
		options.Logger.Printf("gopls: %v", err)
	}
	return runErr
}

type pendingRequest struct {
	method string
	origin *generatedFile
	// nameRange is the range of a name that the generated file does not
	// copy, for a rename that gopls does at a use of the name.
	nameRange *lspRange
}

type server struct {
	generator      Generator
	generateOnSave bool
	logger         *log.Logger
	client         io.Writer
	gopls          io.Writer
	done           chan struct{}
	documents      map[string]*editorDocument
	packages       map[string]*packageState
	// external caches the generated files of packages without open
	// documents. A nil entry is a file that gosling did not generate.
	external map[string]*generatedFile
	// pending maps the ID of each forwarded client request to what the
	// response needs.
	pending          map[string]pendingRequest
	regenerateTimer  *time.Timer
	regenerateSignal chan struct{}
	exiting          bool
}

func newServer(options Options, client, gopls io.Writer) *server {
	return &server{
		generator:        options.Generator,
		generateOnSave:   options.GenerateOnSave,
		logger:           options.Logger,
		client:           client,
		gopls:            gopls,
		done:             make(chan struct{}),
		documents:        map[string]*editorDocument{},
		packages:         map[string]*packageState{},
		external:         map[string]*generatedFile{},
		pending:          map[string]pendingRequest{},
		regenerateSignal: make(chan struct{}, 1),
	}
}

// readMessages reads messages in a goroutine. The channel closes when the
// stream ends or the server stops.
func (server *server) readMessages(reader io.Reader, name string) <-chan *rpcMessage {
	messages := make(chan *rpcMessage)
	go func() {
		defer close(messages)
		buffered := bufio.NewReader(reader)
		for {
			message, err := readMessage(buffered)
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
					server.logger.Printf("read from %s: %v", name, err)
				}
				return
			}
			select {
			case messages <- message:
			case <-server.done:
				return
			}
		}
	}()
	return messages
}

// run handles messages until a stream ends. Only this goroutine reads and
// changes the server state, so the state needs no locks.
func (server *server) run(ctx context.Context, clientMessages, goplsMessages <-chan *rpcMessage) error {
	defer close(server.done)
	defer server.stopTimer()
	for {
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message, ok := <-clientMessages:
			if !ok {
				return nil
			}
			err = server.handleClientMessage(message)
		case message, ok := <-goplsMessages:
			if !ok {
				if server.exiting {
					return nil
				}
				return errors.New("gopls stopped")
			}
			err = server.handleGoplsMessage(message)
		case <-server.regenerateSignal:
			err = server.regenerateDirty()
		}
		if err != nil {
			return err
		}
	}
}

func (server *server) stopTimer() {
	if server.regenerateTimer != nil {
		server.regenerateTimer.Stop()
	}
}

func (server *server) scheduleRegenerate() {
	if server.regenerateTimer != nil {
		server.regenerateTimer.Reset(regenerateDelay)
		return
	}
	server.regenerateTimer = time.AfterFunc(regenerateDelay, func() {
		select {
		case server.regenerateSignal <- struct{}{}:
		default: // a signal is already waiting
		}
	})
}

func (server *server) regenerateDirty() error {
	for _, packageState := range server.packages {
		if !packageState.dirty {
			continue
		}
		if err := server.regenerate(packageState); err != nil {
			return err
		}
	}
	return nil
}

func (server *server) writeClient(message *rpcMessage) error {
	return writeMessage(server.client, message)
}

func (server *server) writeGopls(message *rpcMessage) error {
	return writeMessage(server.gopls, message)
}

func (server *server) notifyClient(method string, params any) error {
	return server.sendNotification(server.client, method, params)
}

func (server *server) notifyGopls(method string, params any) error {
	return server.sendNotification(server.gopls, method, params)
}

func (server *server) sendNotification(writer io.Writer, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return writeMessage(writer, &rpcMessage{Method: method, Params: raw})
}

func (server *server) respondClient(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return server.writeClient(&rpcMessage{ID: id, Result: raw})
}

// golPathOf returns the path of a .gol URI.
func golPathOf(uri string) (string, bool) {
	path, ok := uriToPath(uri)
	if !ok || !strings.HasSuffix(path, sourceExt) {
		return "", false
	}
	return path, true
}

// packageFor returns the state of the directory of a .gol path, and
// transpiles it first if an edit is waiting.
func (server *server) packageFor(golPath string) (*packageState, error) {
	packageState, ok := server.packages[filepath.Dir(golPath)]
	if !ok {
		return nil, nil
	}
	if packageState.dirty {
		if err := server.regenerate(packageState); err != nil {
			return nil, err
		}
	}
	return packageState, nil
}

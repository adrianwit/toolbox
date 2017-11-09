package ssh

import (
	"bytes"
	"fmt"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

const defaultShell = "/bin/bash"

const defautTimeoutMs = 5000


//MultiCommandSession represents a multi command session
type MultiCommandSession interface {

	Run(command string, timeoutMs int, terminators ...string) (string, error);

	ShellPrompt() string

	KernelName()  string

	Close()

}

//multiCommandSession represents a multi command session
//a new command are send vi stdin
type multiCommandSession struct {
	session     *ssh.Session
	stdOutput   chan string
	stdError    chan string
	stdInput    io.WriteCloser
	shellPrompt string
	kernelName  string
	running     int32
}

func (s *multiCommandSession) Run(command string, timeoutMs int, terminators ...string) (string, error) {
	s.drainStdout()
	_, err := s.stdInput.Write([]byte(command + "\n"))
	if err != nil {
		return "", fmt.Errorf("Failed to execute command: %v, err: %v", command, err)
	}
	return s.readResponse(timeoutMs, terminators...)
}


func (s *multiCommandSession) ShellPrompt() string {
	return s.shellPrompt
}

func (s *multiCommandSession) KernelName()  string {
	return s.kernelName
}

//Close closes the session with its resources
func (s *multiCommandSession) Close() {
	atomic.StoreInt32(&s.running, 0)
	s.stdInput.Close()
	s.session.Close()

}

func (s *multiCommandSession) closeIfError(err error) bool {
	if err != nil {
		s.Close()
		return true
	}
	return false
}

func (s *multiCommandSession) init(shell string) (string, error) {
	reader, err := s.session.StdoutPipe()
	if err != nil {
		return "", err
	}
	go s.drain(reader, s.stdOutput)

	errReader, err := s.session.StderrPipe()
	if err != nil {
		return "", err
	}
	go s.drain(errReader, s.stdError)
	if shell == "" {
		shell = defaultShell
	}
	err = s.session.Start(shell)
	if err != nil {
		return "", err
	}
	return s.readResponse(defautTimeoutMs)
}

func (s *multiCommandSession) drain(reader io.Reader, out chan string) {
	var written int64 = 0
	buf := make([]byte, 128*1024)
	for {
		writter := new(bytes.Buffer)
		if atomic.LoadInt32(&s.running) == 0 {
			return
		}

		bytesRead, readError := reader.Read(buf)
		if bytesRead > 0 {
			bytesWritten, writeError := writter.Write(buf[:bytesRead])
			if s.closeIfError(writeError) {
				return
			}
			if bytesWritten > 0 {
				written += int64(bytesWritten)
			}

			if bytesRead != bytesWritten {
				if s.closeIfError(io.ErrShortWrite) {
					return
				}
			}
			out <- string(writter.Bytes())
		}
		if s.closeIfError(readError) {
			return
		}

	}
}

func hasTerminator(source string, terminators ...string) bool {
	for _, candidate := range terminators {
		candidateLen := len(candidate)
		if candidateLen == 0 {
			continue
		}
		if candidate[0:1] == "^" && strings.HasPrefix(source, candidate[1:]) {
			return true
		} else if candidate[candidateLen-1:] == "$" && strings.HasSuffix(source, candidate[:candidateLen-1]) {
			return true
		} else if strings.Contains(source, candidate) {
			return true
		}
	}
	return false
}


func (s *multiCommandSession) readResponse(timeoutMs int, terminators ...string) (out string, err error) {
	if timeoutMs == 0 {
		timeoutMs = defautTimeoutMs
	}
	if len(terminators) == 0 {
		if s.shellPrompt == "" {
			terminators = []string{s.shellPrompt + "$"}
		} else {
			terminators = []string{"$ $"}
		}
	}
	var done int32
	defer atomic.StoreInt32(&done, 1)
	var errOut string
outer:
	for {
		select {

		case o := <-s.stdOutput:
			out += o
			if hasTerminator(out, terminators...) && len(s.stdOutput) == 0 {
				break outer
			}
		case e := <-s.stdError:
			errOut += e
			if hasTerminator(errOut, terminators...) && len(s.stdOutput) == 0 {
				break outer
			}

		case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
			break outer
		}
	}
	if errOut != "" {
		err = errors.New(errOut)
	}
	if len(out) > 0 {
		index := strings.LastIndex(out, "\r\n"+s.shellPrompt)
		if index > 0 {
			out = string(out[:index])
		}
	}
	return out, err
}

func (s *multiCommandSession) drainStdout() {
	//read any outstanding output
	for ; ; {
		out, _ := s.readResponse(1, "")
		if len(out) == 0 {
			return
		}
	}
}


func newMultiCommandSession(client *ssh.Client, config *SessionConfig) (MultiCommandSession, error) {
	if config == nil {
		config = &SessionConfig{}
	}
	config.applyDefault()
	session, err := client.NewSession()
	defer func() {
		if err != nil {
			session.Close()
		}
	}()
	if err != nil {
		return nil, err
	}
	for k, v := range config.EnvVariables {
		err = session.Setenv(k, v)
		if err != nil {
			return nil, err
		}
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // disable echoing
		ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	}

	if err := session.RequestPty(config.Term, config.Rows, config.Columns, modes); err != nil {
		return nil, err
	}
	var writer io.WriteCloser
	writer, err = session.StdinPipe()
	if err != nil {
		return nil, err
	}
	result := &multiCommandSession{
		session:   session,
		stdOutput: make(chan string),
		stdError:  make(chan string),
		stdInput:  writer,
		running:   1,
	}
	_, err = result.init(config.Shell)
	if result.closeIfError(err) {
		return nil, err
	}
	result.shellPrompt, err = result.Run("", 1000)
	if result.closeIfError(err) {
		return nil, err
	}
	result.drainStdout()
	result.kernelName, err = result.Run("uname -s", 20000, "Linux", "Darwin", "$", "#")
	result.drainStdout()
	result.kernelName = strings.TrimSpace(strings.ToLower(result.kernelName))
	return result, err
}

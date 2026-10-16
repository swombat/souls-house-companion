package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type executeInput struct {
	Argv        []string `json:"argv"`
	Cwd         string   `json:"cwd"`
	StdinBase64 string   `json:"stdin_base64,omitempty"`
}

// Input never becomes retained operation metadata or an error string.
func parseExecuteInput(data []byte) (executeInput, []byte, error) {
	var input executeInput
	if len(data) > 8<<20 || !utf8.Valid(data) {
		return input, nil, ErrDenied
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		return executeInput{}, nil, ErrDenied
	}
	if len(input.Argv) == 0 || len(input.Argv) > 1024 || input.Argv[0] == "" ||
		input.Cwd == "" || len(input.Cwd) > 4096 || !filepath.IsAbs(input.Cwd) || strings.ContainsRune(input.Cwd, 0) {
		return executeInput{}, nil, ErrDenied
	}
	total := 0
	for _, arg := range input.Argv {
		total += len(arg)
		if strings.ContainsRune(arg, 0) || len(arg) > 128<<10 {
			return executeInput{}, nil, ErrDenied
		}
	}
	if total > 1<<20 {
		return executeInput{}, nil, ErrDenied
	}
	stdin, err := base64.StdEncoding.Strict().DecodeString(input.StdinBase64)
	if err != nil || len(stdin) > 8<<20 {
		return executeInput{}, nil, ErrDenied
	}
	return input, stdin, nil
}

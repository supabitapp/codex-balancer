package app

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

const configureHelp = `Configure this machine's Codex client.

Usage:
  codex-balancer configure [flags]

Flags:
  -base-url string   balancer model provider URL (default http://127.0.0.1:8317/v1)
  -config string     Codex config file (default $CODEX_HOME/config.toml or ~/.codex/config.toml)
`

type configValue struct {
	path  []string
	value string
	kind  unstable.Kind
}

type configEdit struct {
	start int
	end   int
	value string
	kind  unstable.Kind
}

type configDocument struct {
	assignments map[string]configEdit
	sections    map[string]int
	sectionEnds map[int]int
	rootEnd     int
	paths       [][]string
	lastSection int
}

func codexConfigValues(baseURL string) []configValue {
	return []configValue{
		{path: []string{"model_provider"}, value: strconv.Quote("balancer"), kind: unstable.String},
		{path: []string{"analytics", "enabled"}, value: "false", kind: unstable.Bool},
		{path: []string{"feedback", "enabled"}, value: "false", kind: unstable.Bool},
		{path: []string{"model_providers", "balancer", "name"}, value: strconv.Quote("OpenAI"), kind: unstable.String},
		{path: []string{"model_providers", "balancer", "base_url"}, value: strconv.Quote(baseURL), kind: unstable.String},
		{path: []string{"model_providers", "balancer", "env_key"}, value: strconv.Quote("CODEX_BALANCER_API_KEY"), kind: unstable.String},
		{path: []string{"model_providers", "balancer", "requires_openai_auth"}, value: "true", kind: unstable.Bool},
		{path: []string{"model_providers", "balancer", "supports_websockets"}, value: "true", kind: unstable.Bool},
	}
}

func configureCmd(args []string) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, configureHelp) }
	configPath := fs.String("config", defaultCodexConfigPath(), "Codex config file")
	baseURL := fs.String("base-url", "http://127.0.0.1:8317/v1", "balancer model provider URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("configure takes no arguments")
	}
	normalizedURL, err := validateProviderURL(*baseURL)
	if err != nil {
		return err
	}
	if err := configureCodex(*configPath, normalizedURL); err != nil {
		return err
	}
	fmt.Printf("configured %s\n", *configPath)
	return nil
}

func defaultCodexConfigPath() string {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "config.toml")
	}
	return filepath.Join(homeDir(), ".codex", "config.toml")
}

func validateProviderURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("base URL must be an HTTP or HTTPS URL without credentials, a query, or a fragment")
	}
	return strings.TrimRight(value, "/"), nil
}

func configureCodex(path, baseURL string) error {
	data, mode, err := readCodexConfig(path)
	if err != nil {
		return err
	}
	configured, err := configureCodexDocument(data, baseURL)
	if err != nil {
		return fmt.Errorf("configure %s: %w", path, err)
	}
	if bytes.Equal(data, configured) {
		return nil
	}
	return writeCodexConfig(path, configured, mode)
}

func readCodexConfig(path string) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("stat Codex config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("Codex config is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read Codex config: %w", err)
	}
	return data, info.Mode().Perm(), nil
}

func writeCodexConfig(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Codex config directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create temporary Codex config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set Codex config permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write Codex config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync Codex config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Codex config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace Codex config: %w", err)
	}
	return nil
}

func configureCodexDocument(data []byte, baseURL string) ([]byte, error) {
	values := codexConfigValues(baseURL)
	document, err := parseConfigDocument(data)
	if err != nil {
		return nil, err
	}
	edits := make([]configEdit, 0, len(values)+4)
	missing := map[string][]configValue{}
	for _, value := range values {
		key := configPathKey(value.path)
		if assignment, ok := document.assignments[key]; ok {
			if assignment.kind != value.kind {
				return nil, fmt.Errorf("%s must be %s", strings.Join(value.path, "."), value.kind)
			}
			edits = append(edits, configEdit{start: assignment.start, end: assignment.end, value: value.value})
			continue
		}
		section := value.path[:len(value.path)-1]
		missing[configPathKey(section)] = append(missing[configPathKey(section)], value)
	}

	appendBlocks := strings.Builder{}
	rootInsert := strings.Builder{}
	for _, section := range [][]string{{}, {"analytics"}, {"feedback"}, {"model_providers", "balancer"}} {
		sectionKey := configPathKey(section)
		values := missing[sectionKey]
		if len(values) == 0 {
			continue
		}
		if len(section) == 0 {
			writeAssignments(&rootInsert, values, false)
			continue
		}
		if sectionIndex, ok := document.sections[sectionKey]; ok {
			insert := strings.Builder{}
			writeAssignments(&insert, values, false)
			offset := document.sectionEnds[sectionIndex]
			edits = append(edits, insertionEdit(data, offset, insert.String()))
			continue
		}
		if document.hasPathPrefix(section) {
			writeAssignments(&rootInsert, values, true)
			continue
		}
		appendSection(&appendBlocks, section, values)
	}
	if rootInsert.Len() > 0 && appendBlocks.Len() > 0 && document.rootEnd == len(data) {
		rootInsert.WriteByte('\n')
		rootInsert.WriteString(appendBlocks.String())
		appendBlocks.Reset()
	}
	if rootInsert.Len() > 0 {
		edits = append(edits, insertionEdit(data, document.rootEnd, rootInsert.String()))
	}
	if appendBlocks.Len() > 0 {
		edits = append(edits, insertionEdit(data, len(data), appendBlocks.String()))
	}
	configured, err := applyConfigEdits(data, edits)
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(configured, &map[string]any{}); err != nil {
		return nil, fmt.Errorf("updated config is invalid: %w", err)
	}
	return configured, nil
}

func parseConfigDocument(data []byte) (configDocument, error) {
	document := configDocument{
		assignments: map[string]configEdit{},
		sections:    map[string]int{},
		sectionEnds: map[int]int{},
		rootEnd:     len(data),
		lastSection: -1,
	}
	if err := toml.Unmarshal(data, &map[string]any{}); err != nil {
		return configDocument{}, fmt.Errorf("invalid existing config: %w", err)
	}
	parser := unstable.Parser{}
	parser.Reset(data)
	current := []string{}
	for parser.NextExpression() {
		expression := parser.Expression()
		switch expression.Kind {
		case unstable.Table, unstable.ArrayTable:
			path := configNodeKey(expression)
			iterator := expression.Key()
			iterator.Next()
			start := lineStart(data, int(iterator.Node().Raw.Offset))
			if document.lastSection >= 0 {
				document.sectionEnds[document.lastSection] = start
			}
			if document.lastSection < 0 {
				document.rootEnd = start
			}
			document.lastSection = start
			current = path
			document.paths = append(document.paths, slices.Clone(path))
			if expression.Kind == unstable.Table {
				document.sections[configPathKey(path)] = start
			}
		case unstable.KeyValue:
			path := append(slices.Clone(current), configNodeKey(expression)...)
			document.paths = append(document.paths, slices.Clone(path))
			value := expression.Value()
			document.assignments[configPathKey(path)] = configEdit{
				start: int(value.Raw.Offset),
				end:   int(value.Raw.Offset + value.Raw.Length),
				kind:  value.Kind,
			}
		}
	}
	if err := parser.Error(); err != nil {
		return configDocument{}, fmt.Errorf("invalid existing config: %w", err)
	}
	if document.lastSection >= 0 {
		document.sectionEnds[document.lastSection] = len(data)
	}
	return document, nil
}

func configNodeKey(node *unstable.Node) []string {
	path := []string{}
	iterator := node.Key()
	for iterator.Next() {
		path = append(path, string(iterator.Node().Data))
	}
	return path
}

func configPathKey(path []string) string {
	return strings.Join(path, "\x00")
}

func (d configDocument) hasPathPrefix(prefix []string) bool {
	for _, path := range d.paths {
		if len(path) >= len(prefix) && slices.Equal(path[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func lineStart(data []byte, offset int) int {
	return bytes.LastIndexByte(data[:offset], '\n') + 1
}

func writeAssignments(output *strings.Builder, values []configValue, dotted bool) {
	for _, value := range values {
		path := value.path
		if !dotted {
			path = path[len(path)-1:]
		}
		fmt.Fprintf(output, "%s = %s\n", strings.Join(path, "."), value.value)
	}
}

func appendSection(output *strings.Builder, section []string, values []configValue) {
	if output.Len() > 0 {
		output.WriteByte('\n')
	}
	fmt.Fprintf(output, "[%s]\n", strings.Join(section, "."))
	writeAssignments(output, values, false)
}

func insertionEdit(data []byte, offset int, value string) configEdit {
	if offset > 0 && data[offset-1] != '\n' {
		value = "\n" + value
	}
	if offset == len(data) && offset > 0 && !bytes.HasSuffix(data[:offset], []byte("\n\n")) {
		value = "\n" + value
	}
	return configEdit{start: offset, end: offset, value: value}
}

func applyConfigEdits(data []byte, edits []configEdit) ([]byte, error) {
	slices.SortFunc(edits, func(left, right configEdit) int {
		if left.start != right.start {
			return right.start - left.start
		}
		return right.end - left.end
	})
	result := slices.Clone(data)
	previousStart := len(data)
	for _, edit := range edits {
		if edit.start < 0 || edit.end < edit.start || edit.end > previousStart {
			return nil, errors.New("config edits overlap")
		}
		result = slices.Concat(result[:edit.start], []byte(edit.value), result[edit.end:])
		previousStart = edit.start
	}
	return result, nil
}

package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DirName is the directory that holds workflow files and marks the workflow
// root.
const DirName = ".weft"

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type rawFile struct {
	Concurrency int       `yaml:"concurrency"`
	Shell       string    `yaml:"shell"`
	Nodes       yaml.Node `yaml:"nodes"`
}

type rawNode struct {
	Run             string            `yaml:"run"`
	Needs           []string          `yaml:"needs"`
	Label           string            `yaml:"label"`
	Dir             string            `yaml:"dir"`
	Env             map[string]string `yaml:"env"`
	Shell           string            `yaml:"shell"`
	Interactive     bool              `yaml:"interactive"`
	Timeout         string            `yaml:"timeout"`
	KillTimeout     string            `yaml:"kill_timeout"`
	ContinueOnError bool              `yaml:"continue_on_error"`
}

var nodeFields = []string{
	"run", "needs", "label", "dir", "env", "shell",
	"interactive", "timeout", "kill_timeout", "continue_on_error",
}

// FindRoot walks up from start to the first directory that contains a
// `.weft` directory.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		info, err := os.Stat(filepath.Join(dir, DirName))
		if err == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s directory found in %s or any parent", DirName, start)
		}
		dir = parent
	}
}

// Resolve maps a workflow argument to a file path and workflow root. A name
// such as "deploy" resolves to `<root>/.weft/deploy.yml`. An argument that
// looks like a path (it holds a separator or a YAML extension) is used as
// is; its root is the parent of `.weft` when the file lives in one, and the
// file's directory otherwise.
func Resolve(cwd, arg string) (path, root string, err error) {
	if strings.ContainsRune(arg, filepath.Separator) || isYAML(arg) {
		path = filepath.Clean(arg)
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if _, err := os.Stat(path); err != nil {
			return "", "", fmt.Errorf("workflow file: %w", err)
		}
		root = filepath.Dir(path)
		if filepath.Base(root) == DirName {
			root = filepath.Dir(root)
		}
		return path, root, nil
	}

	root, err = FindRoot(cwd)
	if err != nil {
		return "", "", err
	}
	for _, ext := range []string{".yml", ".yaml"} {
		p := filepath.Join(root, DirName, arg+ext)
		if _, err := os.Stat(p); err == nil {
			return p, root, nil
		}
	}
	return "", "", fmt.Errorf("workflow %q not found in %s", arg, filepath.Join(root, DirName))
}

// List returns the names of the workflows in `<root>/.weft`, sorted.
func List(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, DirName))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
	}
	sort.Strings(names)
	return names, nil
}

func isYAML(name string) bool {
	ext := filepath.Ext(name)
	return ext == ".yml" || ext == ".yaml"
}

// Load reads, parses, and validates the workflow file at path. Errors in the
// file's content come back as a *ValidationError.
func Load(path, root string) (*Workflow, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the workflow the user asked to load
	if err != nil {
		return nil, err
	}
	wf, err := Parse(data)
	if err != nil {
		if verr, ok := errors.AsType[*ValidationError](err); ok {
			verr.Path = path
		}
		return nil, err
	}

	wf.Hash = hash(data)
	wf.Path = path
	wf.Root = root
	wf.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return wf, nil
}

// HashFile returns the hash Load would record for the file at path, so a
// caller can tell whether the file changed since it was loaded.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is a workflow weft already loaded
	if err != nil {
		return "", err
	}
	return hash(data), nil
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Parse parses and validates workflow YAML. Name, Path, Root, and Hash are
// left empty.
func Parse(data []byte) (*Workflow, error) {
	var raw rawFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, &ValidationError{Path: "", Problems: []string{err.Error()}}
	}

	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	wf := &Workflow{
		Name:        "",
		Path:        "",
		Root:        "",
		Hash:        "",
		Concurrency: raw.Concurrency,
		Nodes:       nil,
		index:       map[string]*Node{},
	}
	if wf.Concurrency == 0 {
		wf.Concurrency = DefaultConcurrency
	}
	if wf.Concurrency < 0 {
		addf("concurrency must be positive, got %d", raw.Concurrency)
	}

	defaultShell := slices.Clone(DefaultShell)
	if raw.Shell != "" {
		defaultShell = strings.Fields(raw.Shell)
		if len(defaultShell) == 0 {
			addf("shell must name a program, got %q", raw.Shell)
		}
	}

	switch {
	case raw.Nodes.Kind == 0:
		addf("no nodes defined")
	case raw.Nodes.Kind != yaml.MappingNode:
		addf("line %d: nodes must be a mapping of node IDs", raw.Nodes.Line)
	}

	for i := 0; i+1 < len(raw.Nodes.Content); i += 2 {
		key, value := raw.Nodes.Content[i], raw.Nodes.Content[i+1]
		id := key.Value

		if !idPattern.MatchString(id) {
			addf("line %d: node ID %q must match %s", key.Line, id, idPattern)
		}
		if wf.index[id] != nil {
			addf("line %d: duplicate node %q", key.Line, id)
			continue
		}

		node, nodeProblems := parseNode(id, value, defaultShell)
		problems = append(problems, nodeProblems...)
		wf.Nodes = append(wf.Nodes, node)
		wf.index[id] = node
	}

	if raw.Nodes.Kind == yaml.MappingNode && len(raw.Nodes.Content) == 0 {
		addf("no nodes defined")
	}

	for _, n := range wf.Nodes {
		for i, need := range n.Needs {
			if slices.Contains(n.Needs[:i], need) {
				addf("node %q lists %q twice in needs", n.ID, need)
				continue
			}
			switch {
			case need == n.ID:
				addf("node %q needs itself", n.ID)
			case wf.index[need] == nil:
				addf("node %q needs unknown node %q", n.ID, need)
			}
		}
	}

	if len(problems) == 0 {
		if cycle := findCycle(wf); cycle != nil {
			addf("cycle: %s", strings.Join(cycle, " -> "))
		}
	}

	if len(problems) > 0 {
		return nil, &ValidationError{Path: "", Problems: problems}
	}
	return wf, nil
}

func parseNode(id string, value *yaml.Node, defaultShell []string) (*Node, []string) {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("node %q: ", id)+fmt.Sprintf(format, args...))
	}

	node := &Node{
		ID:              id,
		Label:           "",
		Run:             "",
		Needs:           nil,
		Dir:             "",
		Env:             nil,
		Shell:           slices.Clone(defaultShell),
		Interactive:     false,
		Timeout:         0,
		KillTimeout:     DefaultKillTimeout,
		ContinueOnError: false,
	}

	if value.Kind != yaml.MappingNode {
		addf("line %d: must be a mapping", value.Line)
		return node, problems
	}
	for i := 0; i < len(value.Content); i += 2 {
		if k := value.Content[i]; !slices.Contains(nodeFields, k.Value) {
			addf("line %d: unknown field %q", k.Line, k.Value)
		}
	}

	var raw rawNode
	if err := value.Decode(&raw); err != nil {
		addf("%v", err)
		return node, problems
	}

	node.Label = raw.Label
	node.Run = raw.Run
	node.Needs = raw.Needs
	node.Dir = raw.Dir
	node.Env = raw.Env
	node.Interactive = raw.Interactive
	node.ContinueOnError = raw.ContinueOnError
	if raw.Shell != "" {
		node.Shell = strings.Fields(raw.Shell)
		if len(node.Shell) == 0 {
			addf("shell must name a program, got %q", raw.Shell)
		}
	}

	if strings.TrimSpace(node.Run) == "" {
		addf("run is required")
	}
	if filepath.IsAbs(node.Dir) {
		addf("dir must be relative to the workflow root")
	}
	node.Timeout = parseDuration(raw.Timeout, "timeout", addf)
	if raw.KillTimeout != "" {
		node.KillTimeout = parseDuration(raw.KillTimeout, "kill_timeout", addf)
	}

	return node, problems
}

func parseDuration(s, field string, addf func(string, ...any)) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		addf("%s must be a positive duration like 90s or 5m, got %q", field, s)
		return 0
	}
	return d
}

// findCycle returns one cycle as a path of node IDs that starts and ends on
// the same node, or nil when the graph is acyclic.
func findCycle(wf *Workflow) []string {
	const (
		unvisited = iota
		active
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = active
		stack = append(stack, id)
		for _, need := range wf.index[id].Needs {
			switch state[need] {
			case active:
				start := slices.Index(stack, need)
				return append(slices.Clone(stack[start:]), need)
			case unvisited:
				if c := visit(need); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return nil
	}
	for _, n := range wf.Nodes {
		if state[n.ID] == unvisited {
			if c := visit(n.ID); c != nil {
				return c
			}
		}
	}
	return nil
}

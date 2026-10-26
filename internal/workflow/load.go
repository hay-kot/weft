package workflow

import (
	"cmp"
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

// fieldKind is the YAML shape a field takes.
type fieldKind int

const (
	kindString fieldKind = iota
	kindBool
	kindList
	kindMap
)

var nodeFields = map[string]fieldKind{
	"run": kindString, "needs": kindList, "label": kindString, "dir": kindString,
	"env": kindMap, "shell": kindString, "interactive": kindBool, "timeout": kindString,
	"kill_timeout": kindString, "continue_on_error": kindBool,
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
			return "", fmt.Errorf("no %s directory found in %s or any parent; create %s/<name>.yml, or pass the path to a workflow file", DirName, start, DirName)
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
	dir := filepath.Join(root, DirName)
	if names, err := List(root); err == nil && len(names) > 0 {
		return "", "", fmt.Errorf("workflow %q not found in %s; it has %s", arg, dir, strings.Join(names, ", "))
	}
	return "", "", fmt.Errorf("workflow %q not found in %s, which holds no workflow files", arg, dir)
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
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &ValidationError{Path: "", Problems: []string{err.Error()}}
	}
	if len(doc.Content) == 0 {
		return nil, &ValidationError{Path: "", Problems: []string{"the file is empty; a workflow needs a nodes: mapping with at least one node"}}
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, &ValidationError{Path: "", Problems: []string{fmt.Sprintf("line %d: a workflow must be a mapping with a nodes: key", top.Line)}}
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
		Concurrency: DefaultConcurrency,
		Nodes:       nil,
		index:       map[string]*Node{},
	}
	defaultShell := slices.Clone(DefaultShell)
	var nodes *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, value := top.Content[i], top.Content[i+1]
		switch key.Value {
		case "concurrency":
			var n int
			switch err := value.Decode(&n); {
			case err != nil:
				addf("line %d: concurrency must be a whole number, got %q", value.Line, value.Value)
			case n < 0:
				addf("line %d: concurrency must be positive, got %d", value.Line, n)
			case n > 0:
				wf.Concurrency = n
			}
		case "shell":
			if value.Kind != yaml.ScalarNode {
				addf("line %d: shell must be a string such as \"bash -c\"", value.Line)
				continue
			}
			defaultShell = strings.Fields(value.Value)
			if len(defaultShell) == 0 {
				addf("line %d: shell must name a program, got %q", value.Line, value.Value)
			}
		case "nodes":
			nodes = value
		default:
			addf("line %d: unknown field %q; a workflow has concurrency, shell, and nodes", key.Line, key.Value)
		}
	}

	nodeYAML := map[string]*yaml.Node{}
	switch {
	case nodes == nil, nodes.Kind == yaml.ScalarNode && nodes.Tag == "!!null",
		nodes.Kind == yaml.MappingNode && len(nodes.Content) == 0:
		addf("no nodes defined")
	case nodes.Kind != yaml.MappingNode:
		addf("line %d: nodes must be a mapping of node IDs", nodes.Line)
	default:
		for i := 0; i+1 < len(nodes.Content); i += 2 {
			key, value := nodes.Content[i], nodes.Content[i+1]
			id := key.Value

			if !idPattern.MatchString(id) {
				addf("line %d: node ID %q must match %s", key.Line, id, idPattern)
			}
			if wf.index[id] != nil {
				addf("line %d: duplicate node %q", key.Line, id)
				continue
			}

			node, nodeProblems := parseNode(id, key.Line, value, defaultShell)
			problems = append(problems, nodeProblems...)
			wf.Nodes = append(wf.Nodes, node)
			wf.index[id] = node
			nodeYAML[id] = value
		}
	}

	for _, n := range wf.Nodes {
		for i, need := range n.Needs {
			line := needLine(nodeYAML[n.ID], i)
			switch {
			case slices.Contains(n.Needs[:i], need):
				addf("node %q: line %d: lists %q twice in needs", n.ID, line, need)
			case need == n.ID:
				addf("node %q: line %d: needs itself", n.ID, line)
			case wf.index[need] == nil:
				addf("node %q: line %d: needs unknown node %q", n.ID, line, need)
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

// needLine returns the line of the i-th entry in a node's needs.
func needLine(node *yaml.Node, i int) int {
	needs := fieldValue(node, "needs")
	if needs == nil || i >= len(needs.Content) {
		return node.Line
	}
	return needs.Content[i].Line
}

// fieldValue returns the value of a field in a mapping, or nil.
func fieldValue(m *yaml.Node, field string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == field {
			return m.Content[i+1]
		}
	}
	return nil
}

// kindProblem says what is wrong with a field's value, or "" when its shape
// fits. Checking shapes first keeps YAML decoder messages, which name Go
// types, out of what users see.
func kindProblem(field string, kind fieldKind, v *yaml.Node) string {
	switch kind {
	case kindString:
		if v.Kind != yaml.ScalarNode {
			return field + " must be a string"
		}
	case kindBool:
		if v.Kind != yaml.ScalarNode || v.Tag != "!!bool" {
			return fmt.Sprintf("%s must be true or false, got %q", field, v.Value)
		}
	case kindList:
		ok := v.Kind == yaml.SequenceNode
		for _, item := range v.Content {
			ok = ok && item.Kind == yaml.ScalarNode
		}
		if !ok {
			return field + " must be a list of node IDs, such as [build, test]"
		}
	case kindMap:
		ok := v.Kind == yaml.MappingNode
		for _, item := range v.Content {
			ok = ok && item.Kind == yaml.ScalarNode
		}
		if !ok {
			return field + " must be a mapping of names to values"
		}
	}
	return ""
}

func parseNode(id string, line int, value *yaml.Node, defaultShell []string) (*Node, []string) {
	var problems []string
	addf := func(line int, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("node %q: line %d: ", id, line)+fmt.Sprintf(format, args...))
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
		addf(value.Line, "must be a mapping")
		return node, problems
	}
	lines := map[string]int{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		k, v := value.Content[i], value.Content[i+1]
		kind, known := nodeFields[k.Value]
		if !known {
			addf(k.Line, "unknown field %q", k.Value)
			continue
		}
		if p := kindProblem(k.Value, kind, v); p != "" {
			addf(v.Line, "%s", p)
		}
		lines[k.Value] = v.Line
	}
	if len(problems) > 0 {
		return node, problems
	}

	var raw rawNode
	if err := value.Decode(&raw); err != nil {
		addf(value.Line, "%v", err)
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
			addf(lines["shell"], "shell must name a program, got %q", raw.Shell)
		}
	}

	if strings.TrimSpace(node.Run) == "" {
		addf(cmp.Or(lines["run"], line), "run is required")
	}
	if filepath.IsAbs(node.Dir) {
		addf(lines["dir"], "dir must be relative to the workflow root")
	}
	node.Timeout = parseDuration(raw.Timeout, "timeout", lines["timeout"], addf)
	if raw.KillTimeout != "" {
		node.KillTimeout = parseDuration(raw.KillTimeout, "kill_timeout", lines["kill_timeout"], addf)
	}

	return node, problems
}

func parseDuration(s, field string, line int, addf func(int, string, ...any)) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		addf(line, "%s must be a positive duration like 90s or 5m, got %q", field, s)
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

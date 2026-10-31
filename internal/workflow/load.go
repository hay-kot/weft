package workflow

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

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
// file's directory otherwise. A bare file name that is not in cwd, such as
// `deploy.yml` typed at the project root, is looked up in `.weft/` too.
func Resolve(cwd, arg string) (path, root string, err error) {
	if strings.ContainsRune(arg, filepath.Separator) || isYAML(arg) {
		path = filepath.Clean(arg)
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if _, err := os.Stat(path); err != nil {
			if p, root, ok := inWeftDir(cwd, arg); ok {
				return p, root, nil
			}
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
	dir := filepath.Join(root, DirName)
	var found []string
	for _, ext := range yamlExts {
		p := filepath.Join(dir, arg+ext)
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0], root, nil
	case 2:
		return "", "", fmt.Errorf("workflow %q is both %s and %s in %s; keep one", arg, filepath.Base(found[0]), filepath.Base(found[1]), dir)
	}
	if names, err := List(root); err == nil && len(names) > 0 {
		return "", "", fmt.Errorf("workflow %q not found in %s; it has %s", arg, dir, strings.Join(names, ", "))
	}
	return "", "", fmt.Errorf("workflow %q not found in %s, which holds no workflow files", arg, dir)
}

// inWeftDir finds a bare file name in the nearest `.weft/`.
func inWeftDir(cwd, name string) (path, root string, ok bool) {
	if strings.ContainsRune(name, filepath.Separator) {
		return "", "", false
	}
	root, err := FindRoot(cwd)
	if err != nil {
		return "", "", false
	}
	path = filepath.Join(root, DirName, name)
	if _, err := os.Stat(path); err != nil {
		return "", "", false
	}
	return path, root, true
}

var yamlExts = []string{".yml", ".yaml"}

// List returns the names of the workflows in `<root>/.weft`, sorted. A name
// that has both a .yml and a .yaml file is an error: the two would share a
// run directory, and `weft run <name>` could only ever mean one of them.
func List(root string) ([]string, error) {
	dir := filepath.Join(root, DirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		name, err := Name(e.Name())
		if err != nil {
			return nil, err
		}
		if other, dup := files[name]; dup {
			return nil, fmt.Errorf("%s has both %s and %s; a workflow has one file", dir, other, e.Name())
		}
		files[name] = e.Name()
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Name returns the workflow name of a file: its base name without the
// extension. The name is the run directory's name, so a file whose name is
// only an extension, such as `.yml` or `...yml`, is refused.
func Name(path string) (string, error) {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	switch name {
	case "", ".", "..":
		return "", fmt.Errorf("%s: a workflow file needs a name before its extension, such as deploy.yml", path)
	}
	return name, nil
}

func isYAML(name string) bool {
	return slices.Contains(yamlExts, filepath.Ext(name))
}

// Load reads, parses, and validates the workflow file at path. Errors in the
// file's content come back as a *ValidationError.
func Load(path, root string) (*Workflow, error) {
	name, err := Name(path)
	if err != nil {
		return nil, err
	}
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
	wf.Name = name
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
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

	// A second document would be dropped without a word otherwise, whether
	// it parses or not.
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		addf("line %d: a workflow file holds one YAML document; this one has a second", extra.Line)
	case !errors.Is(err, io.EOF):
		addf("a workflow file holds one YAML document; this one has a second, which does not parse: %v", err)
	}
	rejectAliases(top, addf)

	wf := &Workflow{
		Name:        "",
		Path:        "",
		Root:        "",
		Hash:        "",
		Concurrency: DefaultConcurrency,
		Nodes:       nil,
		index:       map[string]*Node{},
	}
	nodes, defaultShell := parseTop(top, wf, addf)

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

// parseTop reads the top-level fields into wf and returns the nodes mapping
// and the workflow's default shell.
func parseTop(top *yaml.Node, wf *Workflow, addf func(string, ...any)) (nodes *yaml.Node, defaultShell []string) {
	defaultShell = slices.Clone(DefaultShell)
	seen := map[string]int{}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, value := top.Content[i], top.Content[i+1]
		// yaml.Node decoding keeps both copies of a repeated key; taking the
		// last one would lose the first in silence.
		if line, dup := seen[key.Value]; dup {
			addf("line %d: %q is already set on line %d", key.Line, key.Value, line)
			continue
		}
		seen[key.Value] = key.Line
		switch key.Value {
		case "<<":
			addf("line %d: YAML merge keys (<<) are not supported", key.Line)
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
	return nodes, defaultShell
}

// ValidID reports whether id is a well-formed node ID. Node IDs name files in
// the run directory, so anything that reads one back from disk checks it again.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// rejectAliases reports every alias in the tree. Node fields are read from
// yaml.Node shapes, which an alias does not have, so `*base` would surface
// as a confusing "must be a mapping" or "unknown field" otherwise.
func rejectAliases(n *yaml.Node, addf func(string, ...any)) {
	if n.Kind == yaml.AliasNode {
		addf("line %d: YAML anchors and aliases (*%s) are not supported; write the fields out", n.Line, n.Value)
		return
	}
	for _, c := range n.Content {
		rejectAliases(c, addf)
	}
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
		if k.Value == "<<" {
			addf(k.Line, "YAML merge keys (<<) are not supported; write the fields out")
			continue
		}
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
		// Keep a well-formed needs list so its entries are still checked;
		// otherwise an unknown need only shows up once the other problems
		// are fixed.
		if needs := fieldValue(value, "needs"); needs != nil && kindProblem("needs", kindList, needs) == "" {
			for _, item := range needs.Content {
				node.Needs = append(node.Needs, item.Value)
			}
		}
		return node, problems
	}

	var raw rawNode
	if err := value.Decode(&raw); err != nil {
		addf(value.Line, "%v", err)
		return node, problems
	}

	// The label is drawn into one row of the TUI and printed by status; a
	// newline from a block scalar or an escape code would break both.
	node.Label = strings.TrimSpace(raw.Label)
	if strings.ContainsFunc(node.Label, unicode.IsControl) {
		addf(lines["label"], "label must be one line without control characters")
	}
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

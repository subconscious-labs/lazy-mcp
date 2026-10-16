package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// expander resolves ${...} expressions inside YAML string scalars.
type expander struct {
	lookup   func(name string) (string, bool)
	readFile func(path string) ([]byte, error)
}

func defaultExpander() *expander {
	return &expander{
		lookup:   os.LookupEnv,
		readFile: os.ReadFile,
	}
}

// expandNode walks a YAML node tree and expands ${...} inside every
// !!str scalar. Mapping keys are left untouched; only values are
// considered for expansion.
func (e *expander) expandNode(n *yaml.Node) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := e.expandNode(c); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		// Content is [key0, val0, key1, val1, ...]. Skip keys.
		for i := 1; i < len(n.Content); i += 2 {
			if err := e.expandNode(n.Content[i]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return nil
		}
		v, err := e.expand(n.Value)
		if err != nil {
			return err
		}
		if v != n.Value {
			n.Value = v
			retag(n)
		}
	case yaml.AliasNode:
		// Alias points at a node that was already visited.
	}
	return nil
}

// expand replaces ${...} expressions in s.
func (e *expander) expand(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))

	i := 0
	for i < len(s) {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			i++
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i += 2
		case '{':
			// Find the matching closing brace; allow nested braces
			// (used by ${A:-${B}}).
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				switch s[j] {
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						goto found
					}
				}
				j++
			}
			return "", fmt.Errorf("unterminated ${...} in %q", s)
		found:
			inner := s[i+2 : j]
			val, err := e.resolve(inner)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
			i = j + 1
		default:
			b.WriteByte('$')
			i++
		}
	}
	//return b.String(), nil
	out := b.String()
	if strings.Contains(out, "${") {
		return "", fmt.Errorf(
			"expansion of %q produced a value that still contains ${...}: %q - "+
			"check that the referenced environment variable does not contain "+
			"the same template (cyclic reference)",
			s, out)
	}
	return out, nil
}

// resolve handles the inside of a ${...} expression.
func (e *expander) resolve(inner string) (string, error) {
	if strings.HasPrefix(inner, "file:") {
		path := inner[len("file:"):]
		if path == "" {
			return "", fmt.Errorf("empty path in ${file:}")
		}
		data, err := e.readFile(path)
		if err != nil {
			return "", fmt.Errorf("read ${file:%s}: %w", path, err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}

	name := inner
	var def string
	var hasDef bool
	var errMsg string
	var hasErr bool

	if idx := strings.Index(inner, ":-"); idx >= 0 {
		name = inner[:idx]
		def = inner[idx+2:]
		hasDef = true
	} else if idx := strings.Index(inner, ":?"); idx >= 0 {
		name = inner[:idx]
		errMsg = inner[idx+2:]
		hasErr = true
	}

	if !validVarName(name) {
		return "", fmt.Errorf("invalid variable name %q in ${...}", name)
	}

	val, ok := e.lookup(name)
	if !ok || val == "" {
		switch {
		case hasDef:
			// Recurse so defaults may themselves contain expressions.
			return e.expand(def)
		case hasErr:
			if errMsg == "" {
				errMsg = "required"
			}
			return "", fmt.Errorf("%s: %s", name, errMsg)
		default:
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
	}
	return val, nil
}

// validVarName reports whether s is a POSIX-style variable name:
// [A-Za-z_][A-Za-z0-9_]*.
func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// retag re-derives a YAML tag for a scalar whose value changed after
// expansion. Without this, `enabled: ${X}` (X="true") would decode as
// !!str "true" and fail to unmarshal into a bool field.
func retag(n *yaml.Node) {
	switch n.Value {
	case "true", "True", "TRUE", "false", "False", "FALSE":
		n.Tag = "!!bool"
		return
	}
	if _, err := strconv.ParseInt(n.Value, 10, 64); err == nil {
		n.Tag = "!!int"
		return
	}
	if _, err := strconv.ParseFloat(n.Value, 64); err == nil {
		n.Tag = "!!float"
		return
	}
	// Otherwise leave as !!str.
}

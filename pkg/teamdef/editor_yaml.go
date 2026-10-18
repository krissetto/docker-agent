package teamdef

import (
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

const (
	yamlMappingNode  = int(ast.MappingType)
	yamlScalarNode   = int(ast.StringType)
	yamlSequenceNode = int(ast.SequenceType)
)

// yamlEditNode is a narrow mutable view over goccy's source-preserving AST.
// Keeping callbacks on children lets edits replace a scalar without rebuilding
// parent mappings and dropping comments or authored formatting.
type yamlEditNode struct {
	Kind     int
	Value    string
	tag      string
	Content  []*yamlEditNode
	node     ast.Node
	replace  func(ast.Node)
	document *yamlDocument
}

type yamlDocument struct {
	Root *yamlEditNode
	File *ast.File
}

func decodeYAMLNode(data []byte, out *yamlDocument) error {
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return err
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return errors.New("controlled editing requires a single non-empty YAML document")
	}
	root, err := wrapYAMLNode(file.Docs[0].Body, nil)
	if err != nil {
		return err
	}
	if err := rejectAmbiguousYAML(root); err != nil {
		return err
	}
	root.document = out
	out.Root, out.File = root, file
	return nil
}

func wrapYAMLNode(node ast.Node, replace func(ast.Node)) (*yamlEditNode, error) {
	if node == nil {
		return nil, errors.New("nil YAML node")
	}
	wrapped := &yamlEditNode{node: node, replace: replace}
	switch typed := node.(type) {
	case *ast.MappingNode:
		wrapped.Kind = yamlMappingNode
		for _, entry := range typed.Values {
			key, err := wrapYAMLNode(entry.Key, func(next ast.Node) { entry.Key = next.(ast.MapKeyNode) })
			if err != nil {
				return nil, err
			}
			value, err := wrapYAMLNode(entry.Value, func(next ast.Node) { entry.Value = next })
			if err != nil {
				return nil, err
			}
			wrapped.Content = append(wrapped.Content, key, value)
		}
	case *ast.SequenceNode:
		wrapped.Kind = yamlSequenceNode
		for i := range typed.Values {
			index := i
			child, err := wrapYAMLNode(typed.Values[i], func(next ast.Node) { typed.Values[index] = next })
			if err != nil {
				return nil, err
			}
			wrapped.Content = append(wrapped.Content, child)
		}
	case *ast.StringNode:
		wrapped.Kind, wrapped.Value, wrapped.tag = yamlScalarNode, typed.Value, "String"
	case *ast.LiteralNode:
		wrapped.Kind, wrapped.Value, wrapped.tag = yamlScalarNode, typed.Value.Value, "String"
	case ast.ScalarNode:
		wrapped.Kind, wrapped.Value, wrapped.tag = yamlScalarNode, fmt.Sprint(typed.GetValue()), node.Type().String()
	default:
		wrapped.Kind = int(node.Type())
	}
	return wrapped, nil
}

func rejectAmbiguousYAML(node *yamlEditNode) error {
	if node == nil {
		return errors.New("nil YAML node")
	}
	switch typed := node.node.(type) {
	case *ast.AnchorNode, *ast.AliasNode:
		return errors.New("controlled editing does not support YAML aliases or anchors")
	case *ast.TagNode:
		return errors.New("controlled editing does not support YAML tags")
	case *ast.MergeKeyNode:
		return errors.New("controlled editing does not support YAML merge keys")
	case *ast.MappingNode:
		seen := make(map[string]bool, len(typed.Values))
		for _, entry := range typed.Values {
			key, ok := entry.Key.(*ast.StringNode)
			if !ok {
				return errors.New("controlled editing requires string YAML mapping keys")
			}
			if seen[key.Value] {
				return fmt.Errorf("controlled editing does not support duplicate YAML key %q", key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := rejectAmbiguousYAML(child); err != nil {
			return err
		}
	}
	return nil
}

func mappingValue(mapping *yamlEditNode, key string) (*yamlEditNode, bool) {
	if mapping == nil || mapping.Kind != yamlMappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1], true
		}
	}
	return nil, false
}

func scalarMappingValue(mapping *yamlEditNode, key string) (string, bool, error) {
	node, ok := mappingValue(mapping, key)
	if !ok {
		return "", false, nil
	}
	if node.Kind != yamlScalarNode {
		return "", true, fmt.Errorf("%s must be a string scalar", key)
	}
	return node.Value, true, nil
}

func setMappingString(mapping *yamlEditNode, key, value string) {
	if node, ok := mappingValue(mapping, key); ok {
		node.SetString(value)
		return
	}
	_ = appendMapping(mapping, key, scalarAST(value))
}

func (node *yamlEditNode) SetString(value string) {
	if node == nil {
		return
	}
	if current, ok := node.node.(*ast.StringNode); ok {
		encoded := scalarAST(value)
		newString, ok := encoded.(*ast.StringNode)
		if !ok {
			return
		}
		oldOrigin := current.Token.Origin
		leading := oldOrigin[:len(oldOrigin)-len(strings.TrimLeft(oldOrigin, " \t\r\n"))]
		trimmed := strings.TrimRight(oldOrigin[len(leading):], " \t\r\n")
		suffix := oldOrigin[len(leading)+len(trimmed):]
		lexical := strings.TrimLeft(newString.Token.Origin, " \t\r\n")
		newString.Token.Origin = leading + lexical + suffix
		newString.Token.Position = current.Token.Position
		newString.Token.Type = current.Token.Type
		newString.Token.Value = value
		newString.Value = value
		if newString.GetComment() == nil && current.GetComment() != nil {
			_ = newString.SetComment(current.GetComment())
		}
		if node.replace != nil {
			node.replace(newString)
		}
		node.node, node.Kind, node.Value, node.tag = newString, yamlScalarNode, value, "String"
		return
	}
	replacement := scalarAST(value)
	if replacement == nil {
		return
	}
	if node.node != nil && node.node.GetComment() != nil {
		_ = replacement.SetComment(node.node.GetComment())
	}
	shiftNode(replacement, node.node.GetToken().Position.Column)
	if node.replace != nil {
		node.replace(replacement)
	}
	wrapped, err := wrapYAMLNode(replacement, node.replace)
	if err != nil {
		return
	}
	*node = *wrapped
}

func shiftNode(node ast.Node, column int) {
	if node == nil || node.GetToken() == nil || node.GetToken().Position == nil {
		return
	}
	node.AddColumn(column - node.GetToken().Position.Column)
}

func scalarAST(value string) ast.Node {
	encoded, err := yaml.Marshal(map[string]string{"value": value})
	if err != nil {
		return nil
	}
	file, err := parser.ParseBytes(encoded, parser.ParseComments)
	if err != nil || len(file.Docs) != 1 {
		return nil
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok || len(root.Values) != 1 {
		return nil
	}
	return root.Values[0].Value
}

func mustYAMLString(value string) string {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return "''"
	}
	return strings.TrimSpace(string(encoded))
}

func replaceNode(existing, replacement *yamlEditNode) {
	if existing == nil || replacement == nil {
		return
	}
	if replacement.node.GetComment() == nil && existing.node.GetComment() != nil {
		_ = replacement.node.SetComment(existing.node.GetComment())
	}
	shiftNode(replacement.node, existing.node.GetToken().Position.Column)
	if old, ok := existing.node.(*ast.SequenceNode); ok {
		if next, ok := replacement.node.(*ast.SequenceNode); ok {
			next.SetIsFlowStyle(old.IsFlowStyle)
		}
	}
	if existing.replace != nil {
		existing.replace(replacement.node)
	}
	wrapped, err := wrapYAMLNode(replacement.node, existing.replace)
	if err == nil {
		*existing = *wrapped
	}
}

func appendMapping(mapping *yamlEditNode, key string, value ast.Node) error {
	astMapping, ok := mapping.node.(*ast.MappingNode)
	if !ok {
		return errors.New("expected YAML mapping")
	}
	snippet := "" + mustYAMLString(key) + ": " + value.String() + "\n"
	parsed, err := parser.ParseBytes([]byte(snippet), parser.ParseComments)
	if err != nil {
		return err
	}
	entry := parsed.Docs[0].Body.(*ast.MappingNode).Values[0]
	indent := 1
	if len(astMapping.Values) > 0 {
		indent = astMapping.Values[0].Key.GetToken().Position.Column
	}
	entry.AddColumn(indent - entry.Key.GetToken().Position.Column)
	astMapping.Values = append(astMapping.Values, entry)
	keyNode, err := wrapYAMLNode(entry.Key, func(next ast.Node) { entry.Key = next.(ast.MapKeyNode) })
	if err != nil {
		return err
	}
	valueNode, err := wrapYAMLNode(entry.Value, func(next ast.Node) { entry.Value = next })
	if err != nil {
		return err
	}
	mapping.Content = append(mapping.Content, keyNode, valueNode)
	return nil
}

func encodeYAMLNode(root *yamlEditNode) ([]byte, error) {
	if root == nil || root.document == nil || root.document.File == nil {
		return nil, errors.New("missing editable YAML AST")
	}
	return []byte(root.document.File.String()), nil
}

func (node *yamlEditNode) ShortTag() string {
	if node == nil {
		return ""
	}
	return node.tag
}

func appendASTMapping(mapping *ast.MappingNode, key string, value ast.Node) error {
	if value == nil {
		return errors.New("cannot append an invalid YAML value")
	}
	parsed, err := parser.ParseBytes([]byte(mustYAMLString(key)+": "+value.String()+"\n"), parser.ParseComments)
	if err != nil || len(parsed.Docs) != 1 {
		return errors.New("cannot append YAML mapping field")
	}
	entry := parsed.Docs[0].Body.(*ast.MappingNode).Values[0]
	if len(mapping.Values) > 0 {
		entry.AddColumn(mapping.Values[0].Key.GetToken().Position.Column - entry.Key.GetToken().Position.Column)
	}
	mapping.Values = append(mapping.Values, entry)
	return nil
}

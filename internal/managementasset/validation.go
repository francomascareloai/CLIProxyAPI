package managementasset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	"github.com/tdewolff/parse/v2"
	jsparser "github.com/tdewolff/parse/v2/js"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	managementAssetCompatibilityHint   = "__periodFallback"
	managementRollingCompatibilityHint = "rolling_windows_v1"
	managementUsageJournalHint         = "usage_journal_v1"
	managementCreateRootHint           = "createRoot("
	managementRootLookupHint           = `getElementById("root")`
	managementRootLookupHintAlt        = `getElementById('root')`
	managementRootSelectorHint         = `querySelector("#root")`
	managementRootSelectorHintAlt      = `querySelector('#root')`
)

type managementAssetValidationCacheEntry struct {
	err string
}

var managementAssetValidationCache sync.Map

// ValidateManagementHTMLFile validates the management control panel asset stored on disk.
func ValidateManagementHTMLFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return ValidateManagementHTML(data)
}

// ValidateManagementHTML validates the management control panel HTML contract.
func ValidateManagementHTML(data []byte) error {
	sum := sha256.Sum256(data)
	cacheKey := hex.EncodeToString(sum[:])
	if cached, ok := managementAssetValidationCache.Load(cacheKey); ok {
		entry := cached.(managementAssetValidationCacheEntry)
		if entry.err == "" {
			return nil
		}
		return fmt.Errorf("%s", entry.err)
	}

	err := validateManagementHTMLUncached(data)
	entry := managementAssetValidationCacheEntry{}
	if err != nil {
		entry.err = err.Error()
	}
	managementAssetValidationCache.Store(cacheKey, entry)
	return err
}

func validateManagementHTMLUncached(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("management asset is empty")
	}
	for _, marker := range []string{
		usage.UsageAggregatesV2Capability,
		managementRollingCompatibilityHint,
		managementUsageJournalHint,
		managementAssetCompatibilityHint,
	} {
		if !bytes.Contains(data, []byte(marker)) {
			return fmt.Errorf("management asset missing compatibility marker %q", marker)
		}
	}

	hasRoot, scripts, err := extractManagementHTMLComponents(data)
	if err != nil {
		return fmt.Errorf("management asset HTML parse failed: %w", err)
	}
	if !hasRoot {
		return fmt.Errorf("management asset missing div#root")
	}
	if len(scripts) == 0 {
		return fmt.Errorf("management asset missing inline bootstrap script")
	}

	bootstrapFound := false
	for i, script := range scripts {
		trimmed := bytes.TrimSpace(script)
		if len(trimmed) == 0 {
			continue
		}
		if hasManagementBootstrap(trimmed) {
			bootstrapFound = true
		}
		if err := validateInlineManagementScript(trimmed); err != nil {
			return fmt.Errorf("management asset inline script %d invalid: %w", i+1, err)
		}
	}
	if !bootstrapFound {
		return fmt.Errorf("management asset missing React bootstrap for #root")
	}
	return nil
}

func extractManagementHTMLComponents(data []byte) (bool, [][]byte, error) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return false, nil, err
	}

	hasRoot := false
	scripts := make([][]byte, 0, 1)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node == nil {
			return
		}
		if node.Type == html.ElementNode {
			switch node.DataAtom {
			case atom.Div:
				if htmlAttributeEquals(node, "id", "root") {
					hasRoot = true
				}
			case atom.Script:
				if !htmlHasAttribute(node, "src") {
					text := inlineScriptText(node)
					if len(bytes.TrimSpace(text)) > 0 {
						scripts = append(scripts, text)
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return hasRoot, scripts, nil
}

func htmlAttributeEquals(node *html.Node, key string, expected string) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(strings.TrimSpace(attr.Key), key) && attr.Val == expected {
			return true
		}
	}
	return false
}

func htmlHasAttribute(node *html.Node, key string) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(strings.TrimSpace(attr.Key), key) {
			return true
		}
	}
	return false
}

func inlineScriptText(node *html.Node) []byte {
	var builder strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.TextNode {
			builder.WriteString(child.Data)
		}
	}
	return []byte(builder.String())
}

func hasManagementBootstrap(script []byte) bool {
	if !bytes.Contains(script, []byte(managementCreateRootHint)) {
		return false
	}
	return bytes.Contains(script, []byte(managementRootLookupHint)) ||
		bytes.Contains(script, []byte(managementRootLookupHintAlt)) ||
		bytes.Contains(script, []byte(managementRootSelectorHint)) ||
		bytes.Contains(script, []byte(managementRootSelectorHintAlt))
}

func validateInlineManagementScript(script []byte) error {
	program, err := jsparser.Parse(parse.NewInputBytes(script), jsparser.Options{})
	if err != nil {
		return err
	}
	if err := validateTopLevelModuleBindings(program); err != nil {
		return err
	}
	return nil
}

func validateTopLevelModuleBindings(program *jsparser.AST) error {
	if program == nil {
		return fmt.Errorf("empty inline module AST")
	}
	seen := make(map[string]string)
	for _, stmt := range program.List {
		for _, name := range topLevelModuleBindingNames(stmt) {
			if name == "" {
				continue
			}
			if previous, exists := seen[name]; exists {
				return fmt.Errorf("duplicate top-level binding %q (%s conflicts with %s)", name, statementBindingKind(stmt), previous)
			}
			seen[name] = statementBindingKind(stmt)
		}
	}
	return nil
}

func topLevelModuleBindingNames(stmt jsparser.IStmt) []string {
	names := make([]string, 0, 4)
	switch node := stmt.(type) {
	case *jsparser.FuncDecl:
		if node != nil && node.Name != nil {
			names = appendBindingName(names, node.Name.Data)
		}
	case *jsparser.ClassDecl:
		if node != nil && node.Name != nil {
			names = appendBindingName(names, node.Name.Data)
		}
	case *jsparser.ImportStmt:
		if node != nil {
			names = appendBindingName(names, node.Default)
			for _, alias := range node.List {
				if len(alias.Binding) > 0 {
					names = appendBindingName(names, alias.Binding)
					continue
				}
				names = appendBindingName(names, alias.Name)
			}
		}
	case *jsparser.VarDecl:
		if node != nil {
			for _, element := range node.List {
				collectBindingNames(reflect.ValueOf(element.Binding), &names)
			}
		}
	case *jsparser.ExportStmt:
		if node != nil {
			names = append(names, exportBindingNames(node)...)
		}
	}
	return names
}

func exportBindingNames(node *jsparser.ExportStmt) []string {
	if node == nil {
		return nil
	}
	switch decl := node.Decl.(type) {
	case *jsparser.FuncDecl:
		return topLevelModuleBindingNames(decl)
	case *jsparser.ClassDecl:
		return topLevelModuleBindingNames(decl)
	case *jsparser.VarDecl:
		return topLevelModuleBindingNames(decl)
	default:
		return nil
	}
}

func collectBindingNames(value reflect.Value, names *[]string) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return
		}
		if value.CanInterface() {
			switch variable := value.Interface().(type) {
			case *jsparser.Var:
				*names = appendBindingName(*names, variable.Data)
				return
			case jsparser.Var:
				*names = appendBindingName(*names, variable.Data)
				return
			}
		}
		collectBindingNames(value.Elem(), names)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if !field.CanInterface() {
				continue
			}
			collectBindingNames(field, names)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			collectBindingNames(value.Index(i), names)
		}
	}
}

func appendBindingName(names []string, raw []byte) []string {
	name := strings.TrimSpace(string(raw))
	if name == "" {
		return names
	}
	return append(names, name)
}

func statementBindingKind(stmt jsparser.IStmt) string {
	switch stmt.(type) {
	case *jsparser.ImportStmt:
		return "import"
	case *jsparser.FuncDecl:
		return "function declaration"
	case *jsparser.ClassDecl:
		return "class declaration"
	case *jsparser.VarDecl:
		return "variable declaration"
	case *jsparser.ExportStmt:
		return "export declaration"
	default:
		return fmt.Sprintf("%T", stmt)
	}
}

func managementAssetFileCompatible(path string) bool {
	return ValidateManagementHTMLFile(path) == nil
}

func isManagementAssetCompatible(data []byte) bool {
	return ValidateManagementHTML(data) == nil
}

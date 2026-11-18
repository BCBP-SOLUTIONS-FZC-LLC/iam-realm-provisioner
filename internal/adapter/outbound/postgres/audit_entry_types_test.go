package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// emittedEntries names every entry type this service emits as a constant
// audit.Entry literal. The adapter sets EntryType from a variable, which
// platform-audit's audittypecheck cannot see; this table is what
// `make audit-typecheck` checks against Audit Log's taxonomy and the
// producer restrictions (-source-service iam-realm-provisioner).
var emittedEntries = []audit.Entry{
	{EntryType: domain.AuditEntryTypeIdpChanged, SourceService: "iam-realm-provisioner"},
	{EntryType: domain.AuditEntryTypeTermsAccepted, SourceService: "iam-realm-provisioner"},
}

// TestAuditEntryTypes_AreComplete: every AuditEntry* constant declared in
// ../../../core/domain/audit_entry.go+terms.go is in emittedEntries (what audittypecheck checks), so a
// new type cannot skip the check.
func TestAuditEntryTypes_AreComplete(t *testing.T) {
	var inTable []string
	for _, e := range emittedEntries {
		inTable = append(inTable, e.EntryType)
	}
	declared := 0
	for _, file := range []string{"audit_entry.go", "terms.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "..", "core", "domain", file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "AuditEntryType") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					declared++
					if v := strings.Trim(lit.Value, "\"`"); !slices.Contains(inTable, v) {
						t.Errorf("%s (%q) is missing from emittedEntries", name.Name, v)
					}
				}
			}
		}
	}
	if declared != len(emittedEntries) {
		t.Errorf("%d %s* constants declared, %d in emittedEntries", declared, "AuditEntryType", len(emittedEntries))
	}
}

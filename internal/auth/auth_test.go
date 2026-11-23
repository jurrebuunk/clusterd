package auth

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, TokenPrefix) || len(tok) != len(TokenPrefix)+43 {
		t.Fatalf("unexpected token shape %q", tok)
	}
	other, _ := Generate()
	file := "# comment\n\n" + HashLine("laptop", ScopeManage, tok) + "\n" + HashLine("ui", ScopeOperator, other) + "\n"
	tokens, err := Parse(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(tokens)
	got, ok := s.Lookup(tok)
	if !ok || got.Name != "laptop" || got.Scope != ScopeManage {
		t.Fatalf("lookup = %+v, %v", got, ok)
	}
	got, ok = s.Lookup(other)
	if !ok || got.Name != "ui" || got.Scope != ScopeOperator {
		t.Fatalf("lookup other = %+v, %v", got, ok)
	}
	if _, ok := s.Lookup(tok + "x"); ok {
		t.Fatal("modified token accepted")
	}
	if _, ok := s.Lookup(""); ok {
		t.Fatal("empty token accepted")
	}
}

func TestParseErrors(t *testing.T) {
	tok, _ := Generate()
	line := HashLine("a", ScopeRead, tok)
	for name, in := range map[string]string{
		"fields":    "a read",
		"scope":     "a admin sha256:" + strings.Repeat("0", 64),
		"hash":      "a read sha256:zz",
		"algorithm": "a read md5:" + strings.Repeat("0", 64),
		"name":      "A! read sha256:" + strings.Repeat("0", 64),
		"dup name":  line + "\na read sha256:" + strings.Repeat("0", 64),
		"dup hash":  line + "\n" + strings.Replace(line, "a ", "b ", 1),
	} {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestScopes(t *testing.T) {
	if !ScopeManage.Allows(ScopeOperator) || !ScopeOperator.Allows(ScopeRead) {
		t.Fatal("scopes must nest")
	}
	if ScopeRead.Allows(ScopeOperator) || ScopeNone.Allows(ScopeRead) {
		t.Fatal("lower scope allowed a higher one")
	}
	for _, s := range []string{"read", "operator", "manage"} {
		sc, err := ParseScope(s)
		if err != nil || sc.String() != s {
			t.Fatalf("ParseScope(%q) = %v, %v", s, sc, err)
		}
	}
}

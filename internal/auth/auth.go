// Package auth handles API tokens: generation, the hash file the hosts read,
// lookup and scopes.
package auth

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Scope is what a token may do. Scopes nest: Manage includes Operator,
// Operator includes Read.
type Scope int

const (
	ScopeNone Scope = iota
	ScopeRead
	ScopeOperator
	ScopeManage
)

func (s Scope) String() string {
	switch s {
	case ScopeRead:
		return "read"
	case ScopeOperator:
		return "operator"
	case ScopeManage:
		return "manage"
	}
	return "none"
}

// ParseScope turns "read", "operator" or "manage" into a Scope.
func ParseScope(s string) (Scope, error) {
	switch s {
	case "read":
		return ScopeRead, nil
	case "operator":
		return ScopeOperator, nil
	case "manage":
		return ScopeManage, nil
	}
	return ScopeNone, fmt.Errorf("unknown scope %q (want read, operator or manage)", s)
}

// Allows reports whether s covers need.
func (s Scope) Allows(need Scope) bool { return s >= need }

// TokenPrefix marks clusterd tokens, so secret scanners and humans recognise them.
const TokenPrefix = "lab_"

// Generate returns a new random token: the prefix plus 32 random bytes.
func Generate() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func hash(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// HashLine is the line for the token file: "<name> <scope> sha256:<hex>".
func HashLine(name string, scope Scope, token string) string {
	h := hash(token)
	return fmt.Sprintf("%s %s sha256:%s", name, scope, hex.EncodeToString(h[:]))
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidName reports whether name can be used for a token.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Token is one entry of the token file. The plain token is never stored.
type Token struct {
	Name  string
	Scope Scope
	hash  [32]byte
}

// Parse reads a token file. Blank lines and lines starting with # are
// ignored; names and hashes must be unique.
func Parse(r io.Reader) ([]Token, error) {
	var out []Token
	names := map[string]bool{}
	hashes := map[[32]byte]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("line %d: want \"<name> <scope> sha256:<hex>\"", n)
		}
		if !ValidName(f[0]) {
			return nil, fmt.Errorf("line %d: invalid name %q", n, f[0])
		}
		scope, err := ParseScope(f[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		hx, ok := strings.CutPrefix(f[2], "sha256:")
		raw, err := hex.DecodeString(hx)
		if !ok || err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("line %d: invalid hash", n)
		}
		t := Token{Name: f[0], Scope: scope}
		copy(t.hash[:], raw)
		if names[t.Name] {
			return nil, fmt.Errorf("line %d: duplicate name %q", n, t.Name)
		}
		if hashes[t.hash] {
			return nil, fmt.Errorf("line %d: duplicate hash", n)
		}
		names[t.Name], hashes[t.hash] = true, true
		out = append(out, t)
	}
	return out, sc.Err()
}

// Store is the set of valid tokens, reloadable at runtime.
type Store struct {
	path   string
	mu     sync.RWMutex
	tokens []Token
}

// Load reads the token file at path.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	return s, s.Reload()
}

// NewStore builds a store from tokens already in memory (tests).
func NewStore(tokens []Token) *Store { return &Store{tokens: tokens} }

// Reload re-reads the file. On error the old tokens stay in place.
func (s *Store) Reload() error {
	if s.path == "" {
		return errors.New("token store has no file")
	}
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	tokens, err := Parse(f)
	if err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	s.mu.Lock()
	s.tokens = tokens
	s.mu.Unlock()
	return nil
}

// Len is the number of tokens loaded.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens)
}

// Lookup finds the token. It compares against every entry in constant time,
// so timing says nothing about which or how many tokens exist.
func (s *Store) Lookup(token string) (Token, bool) {
	h := hash(token)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found Token
	ok := 0
	for _, t := range s.tokens {
		eq := subtle.ConstantTimeCompare(h[:], t.hash[:])
		if eq == 1 {
			found = t
		}
		ok |= eq
	}
	return found, ok == 1
}

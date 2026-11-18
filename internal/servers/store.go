package servers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/config"
)

var (
	ErrNotFound = errors.New("server not found")
	ErrExists   = errors.New("a server with this ID already exists")
)

// Store keeps server definitions in memory and mirrors them to
// <dir>/<id>.json. Definitions handed out are copies; callers cannot mutate
// stored state.
type Store struct {
	dir     string
	mu      sync.RWMutex
	defs    map[string]*Definition
	invalid map[string]string // file name -> problem
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, defs: map[string]*Definition{}, invalid: map[string]string{}}
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// ParseDefinition decodes, normalizes and validates one definition document.
func ParseDefinition(data []byte) (*Definition, error) {
	d := &Definition{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(d); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	d.Normalize()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

// Load (re)reads every *.json file in the directory. Files that fail to
// parse or validate are skipped and reported through Invalid.
func (s *Store) Load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read servers directory: %w", err)
	}
	defs := map[string]*Definition{}
	invalid := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			invalid[name] = err.Error()
			continue
		}
		d, err := ParseDefinition(data)
		if err != nil {
			invalid[name] = err.Error()
			continue
		}
		if d.ID+".json" != name {
			invalid[name] = fmt.Sprintf("file name must match the server id (%s.json)", d.ID)
			continue
		}
		defs[d.ID] = d
	}
	s.mu.Lock()
	s.defs, s.invalid = defs, invalid
	s.mu.Unlock()
	return nil
}

// Invalid returns the files that could not be loaded and why.
func (s *Store) Invalid() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.invalid))
	for k, v := range s.invalid {
		out[k] = v
	}
	return out
}

func (s *Store) Get(id string) (*Definition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.defs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d.Clone(), nil
}

// List returns all definitions sorted by category, then name.
func (s *Store) List() []*Definition {
	s.mu.RLock()
	out := make([]*Definition, 0, len(s.defs))
	for _, d := range s.defs {
		out = append(out, d.Clone())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !strings.EqualFold(a.Category, b.Category) {
			return strings.ToLower(a.Category) < strings.ToLower(b.Category)
		}
		if !strings.EqualFold(a.Name, b.Name) {
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		return a.ID < b.ID
	})
	return out
}

func (s *Store) write(d *Definition) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	// 0600: definitions may carry sensitive environment variables.
	return config.WriteFileAtomic(s.path(d.ID), append(data, '\n'), 0o600)
}

// Create validates and stores a new definition.
func (s *Store) Create(d *Definition) (*Definition, error) {
	d = d.Clone()
	d.Normalize()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.defs[d.ID]; ok {
		return nil, ErrExists
	}
	if _, err := os.Lstat(s.path(d.ID)); err == nil {
		return nil, fmt.Errorf("%w (a file for this ID exists on disk but failed to load)", ErrExists)
	}
	if err := s.write(d); err != nil {
		return nil, fmt.Errorf("save server definition: %w", err)
	}
	s.defs[d.ID] = d
	return d.Clone(), nil
}

// Update validates and replaces an existing definition. The ID is immutable.
func (s *Store) Update(d *Definition) (*Definition, error) {
	d = d.Clone()
	d.Normalize()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.defs[d.ID]; !ok {
		return nil, ErrNotFound
	}
	if err := s.write(d); err != nil {
		return nil, fmt.Errorf("save server definition: %w", err)
	}
	s.defs[d.ID] = d
	return d.Clone(), nil
}

// Delete removes the definition file. It never touches the server's own
// files or data.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.defs[id]; !ok {
		return ErrNotFound
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete server definition: %w", err)
	}
	delete(s.defs, id)
	return nil
}

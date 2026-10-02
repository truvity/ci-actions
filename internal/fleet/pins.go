// Package fleet answers, for a set of organisations, which versions of the
// shared CI libraries each repository's workflows pin, including the
// TRANSITIVE setup-devbox pin hidden inside a pinned reusable workflow.
//
// A repository that pins `ci-workflows/.github/workflows/check.yaml@<sha>`
// never names setup-devbox itself: that file does, at ITS sha. So the
// answer to "which setup-devbox does this repository run" is found by
// reading the pinned reusable-workflow file at the pinned commit, and the
// `# v1.6.1` comment beside a pin is never trusted: the sha is resolved
// against the library's real tag list.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/truvity/ci-actions/internal/semver"
)

// Source is what the scan needs from GitHub; *Client implements it and
// tests substitute a fake.
type Source interface {
	ListRepos(ctx context.Context, owner string) ([]Repo, error)
	ListDir(ctx context.Context, repo, dir, ref string) ([]string, error)
	File(ctx context.Context, repo, path, ref string) ([]byte, error)
	Tags(ctx context.Context, repo string) ([]Tag, error)
}

// Config selects what to scan and which libraries to judge.
type Config struct {
	Orgs            []string
	WorkflowsRepo   string // default truvity/ci-workflows
	ActionsRepo     string // default truvity/ci-actions
	IncludeArchived bool
	Concurrency     int
}

// Pin is one pin into a shared library found in a repository.
type Pin struct {
	Library string `json:"library"`           // truvity/ci-actions
	Path    string `json:"path"`              // setup-devbox
	SHA     string `json:"sha,omitempty"`     // the pinned commit
	Ref     string `json:"ref,omitempty"`     // the raw ref when it is not a sha
	Version string `json:"version,omitempty"` // the tag the sha IS, resolved, never the comment
	Hint    string `json:"hint,omitempty"`    // the trailing comment, for contradiction reports only
	File    string `json:"file"`              // the workflow in the repository
	Via     string `json:"via,omitempty"`     // empty for a direct pin; else the pinned reusable workflow it was read from
	InTree  bool   `json:"in_tree,omitempty"` // setup-devbox vendored inside a pre-split ci-workflows
}

// SetupDevbox summarises the setup-devbox pins of one repository.
type SetupDevbox struct {
	Lowest   string `json:"lowest"` // a tag, "untagged:<sha12>", or "in-tree <ci-workflows tag>" (pre-split)
	BelowMin bool   `json:"below_min"`
	Pins     []Pin  `json:"pins"`
}

// RepoResult is the answer for one repository.
type RepoResult struct {
	Repo          string       `json:"repo"`
	DefaultBranch string       `json:"default_branch"`
	Archived      bool         `json:"archived,omitempty"`
	CIWorkflows   []string     `json:"ci_workflows"`
	CIActions     []string     `json:"ci_actions"`
	SetupDevbox   *SetupDevbox `json:"setup_devbox,omitempty"`
	Pins          []Pin        `json:"pins"`
	Error         string       `json:"error,omitempty"`
}

// Report is the whole scan.
type Report struct {
	Orgs           []string     `json:"orgs"`
	WorkflowsRepo  string       `json:"workflows_repo"`
	ActionsRepo    string       `json:"actions_repo"`
	MinSetupDevbox string       `json:"min_setup_devbox,omitempty"`
	Repos          []RepoResult `json:"repos"`
	Below          []string     `json:"below,omitempty"`
	Errors         []string     `json:"errors,omitempty"`
}

type scanner struct {
	cfg  Config
	src  Source
	tags map[string]map[string][]string // library -> sha -> tag names

	mu    sync.Mutex
	files map[string]*fileEntry
}

type fileEntry struct {
	once sync.Once
	data []byte
	err  error
}

// Scan reads every repository of cfg.Orgs.
func Scan(ctx context.Context, src Source, cfg Config) (*Report, error) {
	if cfg.WorkflowsRepo == "" {
		cfg.WorkflowsRepo = "truvity/ci-workflows"
	}
	if cfg.ActionsRepo == "" {
		cfg.ActionsRepo = "truvity/ci-actions"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	s := &scanner{cfg: cfg, src: src, tags: map[string]map[string][]string{}, files: map[string]*fileEntry{}}
	for _, lib := range []string{cfg.WorkflowsRepo, cfg.ActionsRepo} {
		list, err := src.Tags(ctx, lib)
		if err != nil {
			return nil, fmt.Errorf("reading tags of %s: %w", lib, err)
		}
		m := map[string][]string{}
		for _, t := range list {
			m[t.Commit] = append(m[t.Commit], t.Name)
		}
		s.tags[lib] = m
	}

	var repos []Repo
	for _, org := range cfg.Orgs {
		list, err := src.ListRepos(ctx, org)
		if err != nil {
			return nil, fmt.Errorf("listing repositories of %s: %w", org, err)
		}
		for _, r := range list {
			if r.Archived && !cfg.IncludeArchived {
				continue
			}
			repos = append(repos, r)
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })

	results := make([]RepoResult, len(repos))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r Repo) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.scanRepo(ctx, r)
		}(i, r)
	}
	wg.Wait()

	rep := &Report{Orgs: cfg.Orgs, WorkflowsRepo: cfg.WorkflowsRepo, ActionsRepo: cfg.ActionsRepo, Repos: results}
	for _, r := range results {
		if r.Error != "" {
			rep.Errors = append(rep.Errors, r.Repo+": "+r.Error)
		}
	}
	return rep, nil
}

func (s *scanner) cached(ctx context.Context, repo, p, ref string) ([]byte, error) {
	key := repo + "|" + p + "|" + ref
	s.mu.Lock()
	e := s.files[key]
	if e == nil {
		e = &fileEntry{}
		s.files[key] = e
	}
	s.mu.Unlock()
	e.once.Do(func() { e.data, e.err = s.src.File(ctx, repo, p, ref) })
	return e.data, e.err
}

func isWorkflowFile(p string) bool {
	return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")
}

func (s *scanner) scanRepo(ctx context.Context, r Repo) RepoResult {
	res := RepoResult{Repo: r.FullName, DefaultBranch: r.DefaultBranch, Archived: r.Archived, CIWorkflows: []string{}, CIActions: []string{}, Pins: []Pin{}}
	files, err := s.src.ListDir(ctx, r.FullName, ".github/workflows", r.DefaultBranch)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for _, f := range files {
		if !isWorkflowFile(f) {
			continue
		}
		data, err := s.src.File(ctx, r.FullName, f, r.DefaultBranch)
		if err != nil {
			res.Error = fmt.Sprintf("reading %s: %v", f, err)
			return res
		}
		for _, u := range ParseUses(string(data)) {
			pins, err := s.pinsOf(ctx, u, f, "", 0, map[string]bool{})
			if err != nil {
				res.Error = fmt.Sprintf("resolving %s in %s: %v", u.FullRepo()+"/"+u.Path, f, err)
				return res
			}
			res.Pins = appendUnique(res.Pins, pins...)
		}
	}
	summarise(&res, s.cfg)
	return res
}

// pinsOf turns one `uses:` into the pins it carries: itself when it is
// into a shared library, and, for a pinned reusable workflow of the
// workflows library, whatever that file pins at its own sha.
func (s *scanner) pinsOf(ctx context.Context, u Use, file, via string, depth int, seen map[string]bool) ([]Pin, error) {
	lib := u.FullRepo()
	if lib != s.cfg.WorkflowsRepo && lib != s.cfg.ActionsRepo {
		return nil, nil
	}
	pin := Pin{Library: lib, Path: u.Path, Hint: u.Hint, File: file, Via: via}
	if u.IsSHA() {
		pin.SHA = u.Ref
		pin.Version = s.tagFor(lib, u.Ref)
	} else {
		pin.Ref = u.Ref
		if v, ok := semver.Parse(u.Ref); ok {
			pin.Version = v.String()
		}
	}
	// Before the split setup-devbox lived INSIDE ci-workflows, pinned as
	// ci-workflows/.github/actions/setup-devbox. It is the same action at
	// a far older version, so it is judged as setup-devbox, not as a pin
	// into the workflows library.
	if lib == s.cfg.WorkflowsRepo && strings.HasPrefix(u.Path, ".github/actions/setup-devbox") {
		pin.Library, pin.Path, pin.InTree = s.cfg.ActionsRepo, "setup-devbox", true
		return []Pin{pin}, nil
	}
	out := []Pin{pin}

	// The transitive step: read the pinned reusable workflow at its pin.
	if lib == s.cfg.WorkflowsRepo && strings.HasPrefix(u.Path, ".github/workflows/") && isWorkflowFile(u.Path) && depth < 3 {
		key := lib + "|" + u.Path + "|" + u.Ref
		if seen[key] {
			return out, nil
		}
		seen[key] = true
		inner, err := s.cached(ctx, lib, u.Path, u.Ref)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("pinned file %s/%s not found at %s", lib, u.Path, short(u.Ref))
			}
			return nil, err
		}
		innerVia := fmt.Sprintf("%s/%s@%s", lib, u.Path, firstNonEmpty(pin.Version, short(u.Ref)))
		for _, iu := range ParseUses(string(inner)) {
			switch {
			case iu.Local != "":
				// ./.github/workflows/other.yaml inside the pinned file is
				// the same library at the same ref. A vendored
				// ./.github/actions/setup-devbox is the PRE-SPLIT shape,
				// where setup-devbox lived in ci-workflows itself.
				p := strings.TrimPrefix(iu.Local, "./")
				if strings.HasPrefix(p, ".github/actions/setup-devbox") {
					out = append(out, Pin{Library: s.cfg.ActionsRepo, Path: "setup-devbox", Version: pin.Version, SHA: pin.SHA, InTree: true, File: file, Via: innerVia})
					continue
				}
				if strings.HasPrefix(p, ".github/workflows/") && isWorkflowFile(p) {
					sub := Use{Owner: u.Owner, Repo: u.Repo, Path: p, Ref: u.Ref}
					more, err := s.pinsOf(ctx, sub, file, innerVia, depth+1, seen)
					if err != nil {
						return nil, err
					}
					// the nested file's own pin on the library is not a pin of the repository
					out = append(out, more[1:]...)
				}
			default:
				more, err := s.pinsOf(ctx, iu, file, innerVia, depth+1, seen)
				if err != nil {
					return nil, err
				}
				out = append(out, more...)
			}
		}
	}
	return out, nil
}

// appendUnique drops exact repeats: a workflow that uses the same action
// in three jobs pins it once.
func appendUnique(dst []Pin, more ...Pin) []Pin {
	for _, p := range more {
		dup := false
		for _, q := range dst {
			if p == q {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, p)
		}
	}
	return dst
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// tagFor names the release a commit IS, highest version first when a
// commit carries several tags. Empty when the commit is no release.
func (s *scanner) tagFor(lib, sha string) string {
	names := s.tags[lib][sha]
	if len(names) == 0 {
		return ""
	}
	sorted := append([]string(nil), names...)
	sort.Slice(sorted, func(i, j int) bool { return semver.VersionSortLess(sorted[j], sorted[i]) })
	return sorted[0]
}

func summarise(res *RepoResult, cfg Config) {
	wf, ac := map[string]bool{}, map[string]bool{}
	var setup []Pin
	for _, p := range res.Pins {
		label := pinLabel(p)
		if p.Via == "" {
			switch p.Library {
			case cfg.WorkflowsRepo:
				wf[label] = true
			case cfg.ActionsRepo:
				ac[label] = true
			}
		}
		if p.Library == cfg.ActionsRepo && p.Path == "setup-devbox" {
			setup = append(setup, p)
		}
	}
	res.CIWorkflows = sortedKeys(wf)
	res.CIActions = sortedKeys(ac)
	if len(setup) > 0 {
		sd := &SetupDevbox{Pins: setup}
		sd.Lowest = lowest(setup)
		res.SetupDevbox = sd
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return semver.VersionSortLess(out[i], out[j]) })
	return out
}

// pinLabel is how a setup-devbox pin is shown.
func pinLabel(p Pin) string {
	switch {
	case p.InTree:
		return "in-tree " + firstNonEmpty(p.Version, short(firstNonEmpty(p.SHA, p.Ref)))
	case p.Version != "":
		return p.Version
	default:
		return "untagged:" + short(firstNonEmpty(p.SHA, p.Ref))
	}
}

// rank orders pins for "lowest": a pin that names no release, or no
// version at all, is below every release, because it cannot be shown to
// be at or above anything.
func rank(p Pin) (semver.Version, bool) {
	if p.InTree || p.Version == "" {
		return semver.Version{}, false
	}
	return semver.Parse(p.Version)
}

func lowest(pins []Pin) string {
	best := pins[0]
	for _, p := range pins[1:] {
		if less(p, best) {
			best = p
		}
	}
	return pinLabel(best)
}

func less(a, b Pin) bool {
	va, oka := rank(a)
	vb, okb := rank(b)
	switch {
	case !oka && !okb:
		return pinLabel(a) < pinLabel(b)
	case !oka:
		return true
	case !okb:
		return false
	}
	return va.Compare(vb) < 0
}

// ApplyGate marks every repository whose lowest setup-devbox pin is below
// min (a vX.Y.Z tag) and returns the repositories below it. A pin that
// resolves to no release counts as below: the gate cannot prove it is not.
func ApplyGate(rep *Report, min string) ([]string, error) {
	mv, ok := semver.Parse(min)
	if !ok {
		return nil, fmt.Errorf("--min-setup-devbox %q is not a version like v1.6.1", min)
	}
	rep.MinSetupDevbox = mv.String()
	rep.Below = nil
	for i := range rep.Repos {
		r := &rep.Repos[i]
		if r.SetupDevbox == nil {
			continue
		}
		for _, p := range r.SetupDevbox.Pins {
			v, ok := rank(p)
			if !ok || v.Compare(mv) < 0 {
				r.SetupDevbox.BelowMin = true
				break
			}
		}
		if r.SetupDevbox.BelowMin {
			rep.Below = append(rep.Below, r.Repo)
		}
	}
	return rep.Below, nil
}

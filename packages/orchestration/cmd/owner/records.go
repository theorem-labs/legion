package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func (s *session) needAt(source, target string) ([]byte, ownerpkg.NeedFront, error) {
	file := ownerpkg.NeedRecord(s.config.RecordsRoot, source, target)
	data, found, err := s.blob(branch(source), file)
	if err != nil {
		return nil, ownerpkg.NeedFront{}, err
	}
	if !found {
		return nil, ownerpkg.NeedFront{}, refusef("%s has no need record %s", source, file)
	}
	front, _, err := parseNeedRecord(data)
	if err != nil {
		return nil, ownerpkg.NeedFront{}, fmt.Errorf("read %s: %w", file, err)
	}
	return data, front, nil
}

func (s *session) spec(owner string) ([]byte, ownerpkg.SpecFront, []ownerpkg.Claim, bool, error) {
	file := ownerpkg.SpecRecord(s.config.RecordsRoot, owner)
	data, found, err := s.blob(branch(owner), file)
	if err != nil || !found {
		return nil, ownerpkg.SpecFront{}, nil, false, err
	}
	front, claims, err := ownerpkg.ParseSpec(data)
	if err != nil {
		return nil, ownerpkg.SpecFront{}, nil, false, fmt.Errorf("read %s's spec: %w", owner, err)
	}
	return data, front, claims, true, nil
}

// reviewers are the models of owner's review passes over one revision of subject (reviews/<revision>-<pass>.yaml).
// An owner with no reviews directory yet has none; a reviews path that git cannot list is unable to check.
func (s *session) reviewers(owner, subject string, revision int) ([]string, error) {
	dir := ownerpkg.NodeRecord(s.config.RecordsRoot, owner, "reviews")
	tree := branch(owner) + ":" + dir
	if _, err := s.git("rev-parse", "--verify", "--quiet", tree); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return []string{}, nil
		}
		return nil, unable(fmt.Errorf("find the reviews of %s: %w", owner, err))
	}
	out, err := s.git("ls-tree", "--name-only", tree)
	if err != nil {
		return nil, unable(fmt.Errorf("list the reviews of %s: %w", owner, err))
	}
	models := []string{}
	for _, name := range strings.Fields(out) {
		if !strings.HasPrefix(name, strconv.Itoa(revision)+"-") {
			continue
		}
		data, _, err := s.blob(branch(owner), path.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var review ownerpkg.Review
		if err := review.Unmarshal(data); err != nil {
			return nil, fmt.Errorf("read review %s: %w", name, err)
		}
		if review.Subject == subject && review.Revision == revision && !containsString(models, review.Model) {
			models = append(models, review.Model)
		}
	}
	sort.Strings(models)
	return models, nil
}

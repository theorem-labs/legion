package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"gopkg.in/yaml.v3"
)

// needInput is a need draft: the owner writes the claim tree, the claims it cites and the requirements it resolves;
// the command sets the edge, source, target, origin, revision, epoch and retired ids.
type needInput struct {
	AuthorModel string
	Cites       []string
	Resolves    []string
	Body        []byte
	Claims      []ownerpkg.Claim
}

func readNeedDraft(file string) (needInput, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return needInput{}, refusef("read the need draft: %v", err)
	}
	front, body, err := splitFrontMatter(data)
	if err != nil {
		return needInput{}, refusef("need draft %s: %v", file, err)
	}
	var fields struct {
		AuthorModel string   `yaml:"author_model"`
		Cites       []string `yaml:"cites"`
		Resolves    []string `yaml:"resolves"`
	}
	if err := strictYAML(front, &fields); err != nil {
		return needInput{}, refusef("need draft %s front matter (author_model, cites, resolves; the command sets source, target, origin, revision, epoch and retired): %v", file, err)
	}
	if fields.Cites == nil {
		fields.Cites = []string{}
	}
	if fields.Resolves == nil {
		fields.Resolves = []string{}
	}
	claims, err := parseClaimTree(body)
	if err != nil {
		return needInput{}, refusef("need draft %s: %v", file, err)
	}
	return needInput{AuthorModel: fields.AuthorModel, Cites: fields.Cites, Resolves: fields.Resolves, Body: body, Claims: claims}, nil
}

// specInput is a spec draft: the owner writes the claim tree, whether the spec is complete and the requirements it
// resolves; the command sets the owner, revision, epoch, answers and retired ids.
type specInput struct {
	AuthorModel string
	Complete    bool
	Resolves    []string
	Body        []byte
	Claims      []ownerpkg.Claim
}

func readSpecDraft(file, owner string) (specInput, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return specInput{}, refusef("read the spec draft: %v", err)
	}
	front, body, err := splitFrontMatter(data)
	if err != nil {
		return specInput{}, refusef("spec draft %s: %v", file, err)
	}
	var fields struct {
		Owner       string   `yaml:"owner"`
		AuthorModel string   `yaml:"author_model"`
		Complete    bool     `yaml:"complete"`
		Resolves    []string `yaml:"resolves"`
	}
	if err := strictYAML(front, &fields); err != nil {
		return specInput{}, refusef("spec draft %s front matter (author_model, complete, resolves; the command sets owner, revision, epoch, answers and retired): %v", file, err)
	}
	if fields.Owner != "" && fields.Owner != owner {
		return specInput{}, refusef("spec draft %s is for %s, not %s", file, fields.Owner, owner)
	}
	claims, err := parseClaimTree(body)
	if err != nil {
		return specInput{}, refusef("spec draft %s: %v", file, err)
	}
	draft := specInput{AuthorModel: fields.AuthorModel, Complete: fields.Complete, Resolves: fields.Resolves, Body: body, Claims: claims}
	if draft.Resolves == nil {
		draft.Resolves = []string{}
	}
	return draft, nil
}

type replyDraft struct {
	fields map[string]string
	text   string
}

// readReplyDraft reads a reply's text, after optional front matter with counterexample and bugs_row.
func readReplyDraft(file string) (replyDraft, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return replyDraft{}, refusef("read the reply draft: %v", err)
	}
	draft := replyDraft{fields: map[string]string{}}
	body := data
	if bytes.HasPrefix(data, []byte("---\n")) {
		front, rest, err := splitFrontMatter(data)
		if err != nil {
			return replyDraft{}, refusef("reply draft %s: %v", file, err)
		}
		var fields struct {
			Counterexample string `yaml:"counterexample"`
			BugsRow        string `yaml:"bugs_row"`
		}
		if err := strictYAML(front, &fields); err != nil {
			return replyDraft{}, refusef("reply draft %s front matter (counterexample, bugs_row): %v", file, err)
		}
		draft.fields["counterexample"], draft.fields["bugs_row"] = fields.Counterexample, fields.BugsRow
		body = rest
	}
	draft.text = strings.TrimSpace(string(body))
	if draft.text == "" {
		return replyDraft{}, refusef("reply draft %s has no text", file)
	}
	return draft, nil
}

func strictYAML(data []byte, dest any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(dest); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func splitFrontMatter(data []byte) ([]byte, []byte, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, nil, errors.New("front matter must start with a --- line")
	}
	rest := data[len("---\n"):]
	if bytes.HasPrefix(rest, []byte("---\n")) {
		return nil, rest[len("---\n"):], nil
	}
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return nil, nil, errors.New("front matter must end with a --- line")
	}
	return rest[:end+1], rest[end+len("\n---\n"):], nil
}

// parseClaimTree checks a claim tree with the spec parser, which owns the claim grammar.
func parseClaimTree(body []byte) ([]ownerpkg.Claim, error) {
	_, claims, err := ownerpkg.ParseSpec(append([]byte("---\nowner: draft\nrevision: 1\nepoch: 1\nauthor_model: draft\n---\n"), body...))
	return claims, err
}

func marshalNeedRecord(front ownerpkg.NeedFront, body []byte) ([]byte, error) {
	data, err := front.Marshal()
	if err != nil {
		return nil, err
	}
	return frontMatterRecord(data, body), nil
}

func parseNeedRecord(data []byte) (ownerpkg.NeedFront, []byte, error) {
	front, body, err := splitFrontMatter(data)
	if err != nil {
		return ownerpkg.NeedFront{}, nil, err
	}
	var need ownerpkg.NeedFront
	if err := need.Unmarshal(front); err != nil {
		return ownerpkg.NeedFront{}, nil, err
	}
	return need, body, nil
}

func marshalSpec(front ownerpkg.SpecFront, body []byte) ([]byte, error) {
	data, err := yaml.Marshal(front)
	if err != nil {
		return nil, err
	}
	return frontMatterRecord(data, body), nil
}

func frontMatterRecord(front, body []byte) []byte {
	out := make([]byte, 0, len(front)+len(body)+8)
	out = append(out, "---\n"...)
	out = append(out, front...)
	out = append(out, "---\n"...)
	return append(out, body...)
}

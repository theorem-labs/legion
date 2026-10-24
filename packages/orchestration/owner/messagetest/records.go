package messagetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// The trailers of a saw record's commit: the Envoy event id of the delivery and the topic it came on. An answer
// record's commit carries the step runner's Owner-Item trailer.
const (
	DeliveryTrailer = "Owner-Delivery"
	TopicTrailer    = "Owner-Topic"
	itemTrailer     = "Owner-Item"
)

// AnswerText is the fixed text of every answer record.
const AnswerText = "message test answer"

// SubscribedRecord is the path of the record that lists the topics an owner subscribed to.
func SubscribedRecord(root, owner string) string {
	return ownerpkg.NodeRecord(root, owner, "message-test", "subscribed.yaml")
}

// SawRecord is the path of an owner's record of one delivery, named by its Envoy event id.
func SawRecord(root, owner, delivery string) string {
	return ownerpkg.NodeRecord(root, owner, "message-test", "saw", ownerpkg.Hex16(delivery)+".yaml")
}

// AnswerRecord is the path of an owner's answer to the items of one type and subject: each newer version rewrites
// it, so the version it holds is the newest the owner answered.
func AnswerRecord(root, owner, itemType, subject string) string {
	return ownerpkg.NodeRecord(root, owner, "message-test", "answers", ownerpkg.Hex16(itemType+":"+subject)+".yaml")
}

// Subscribed lists the topics an owner subscribed to before the driver publishes.
type Subscribed struct {
	Owner  string   `yaml:"owner"`
	Topics []string `yaml:"topics"`
}

// Saw is one delivery an owner received. ReceivedAt is the owner's clock when it ran `saw`.
type Saw struct {
	Owner      string    `yaml:"owner"`
	Delivery   string    `yaml:"delivery"`
	Topic      string    `yaml:"topic"`
	ReceivedAt time.Time `yaml:"received_at"`
}

// Answer is an owner's answer to one item: an offer record with fixed text.
type Answer struct {
	Item    string `yaml:"item"`
	Type    string `yaml:"type"`
	Subject string `yaml:"subject"`
	Version string `yaml:"version"`
	Text    string `yaml:"text"`
}

// ExtraTopicsPath is the driver's list, for each test owner, of the topics it subscribes to beyond `owner topics`: the
// fact and concept topics that no node owner holds.
func ExtraTopicsPath(stateDir string) string {
	return filepath.Join(stateDir, "message-test", "topics.json")
}

// LoadExtraTopics reads ExtraTopicsPath. A deployment with no message test has none.
func LoadExtraTopics(stateDir string) (map[string][]string, error) {
	data, err := os.ReadFile(ExtraTopicsPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	topics := map[string][]string{}
	if err := json.Unmarshal(data, &topics); err != nil {
		return nil, fmt.Errorf("decode %s: %w", ExtraTopicsPath(stateDir), err)
	}
	return topics, nil
}

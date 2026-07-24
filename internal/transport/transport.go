package transport

import (
	"context"
	"errors"
	"strings"
)

const (
	Telegram = "telegram"
	Discord  = "discord"
)

type Address struct {
	Transport      string `json:"transport"`
	ConversationID string `json:"conversation_id"`
}

func (a Address) Valid() bool {
	return strings.TrimSpace(a.Transport) != "" && strings.TrimSpace(a.ConversationID) != ""
}

func (a Address) Key() string {
	return a.Transport + ":" + a.ConversationID
}

func (a Address) String() string {
	return a.Key()
}

type Inbound struct {
	EventID      string
	ResponseID   string
	Sequence     int64
	Address      Address
	UserID       string
	Private      bool
	Text         string
	Action       *Action
	Autocomplete *Autocomplete
}

type Action struct {
	ID   string
	Data string
}

type Button struct {
	Text string
	Data string
}

type Autocomplete struct {
	ID      string
	Command string
	Option  string
	Query   string
}

type Choice struct {
	Name  string
	Value string
}

type Health struct {
	State  string
	Detail string
}

type Sink func(context.Context, Inbound) error

type Sender interface {
	Name() string
	MaxMessageLength() int
	Send(context.Context, string, string) error
	SendKeyboard(context.Context, string, string, [][]Button) error
	AnswerAction(context.Context, string, string) error
}

type Source interface {
	Sender
	Run(context.Context, Sink) error
}

type StatusEditor interface {
	CreateStatus(context.Context, string, string, [][]Button) (string, error)
	EditStatus(context.Context, string, string, string, [][]Button) error
}

type ResponseSender interface {
	SendResponse(context.Context, string, string, [][]Button) error
}

type AutocompleteResponder interface {
	AnswerAutocomplete(context.Context, string, []Choice) error
}

type HealthReporter interface {
	Health() Health
}

type Prober interface {
	Probe(context.Context) error
}

var ErrUnsupported = errors.New("transport operation is unsupported")

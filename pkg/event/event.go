package event

import (
	"time"
)

var (
	EventError string = "error"
)

type Data interface{}

// Event base event
type Event struct {
	Name string
	Type string
	Time time.Time
	From string
	Data Data
}

func NewErrorEvent(from, msg string) *Event {
	e := new(Event)
	e.Name = msg
	e.Type = EventError
	e.From = from
	e.Time = time.Now()
	return e
}

func NewEvent(name, strType, from string, data Data) *Event {
	e := new(Event)
	e.Name = name
	e.Type = strType
	e.From = from
	e.Data = data
	e.Time = time.Now()
	return e
}

func (e *Event) GetName() string {
	return e.Name
}

func (e *Event) GetType() string {
	return e.Type
}

func (e *Event) GetTime() time.Time {
	return e.Time
}

func (e *Event) GetFrom() string {
	return e.From
}

func (e *Event) GetData() Data {
	return e.Data
}

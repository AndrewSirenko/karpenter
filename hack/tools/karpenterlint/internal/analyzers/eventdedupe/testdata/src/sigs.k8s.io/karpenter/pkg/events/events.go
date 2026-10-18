package events

type Event struct {
	Reason       string
	Message      string
	DedupeValues []string
}

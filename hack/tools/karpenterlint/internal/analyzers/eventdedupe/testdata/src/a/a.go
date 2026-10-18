package a

import "sigs.k8s.io/karpenter/pkg/events"

func ok(name string) events.Event {
	return events.Event{Reason: "Drained", DedupeValues: []string{name}}
}

func missing() events.Event {
	return events.Event{Reason: "AwaitingVolumeDetachment", Message: "awaiting"} // want `events.Event without DedupeValues`
}

func slice(name string) []events.Event {
	return []events.Event{
		{Reason: "A", DedupeValues: []string{name}},
		{Reason: "B"}, // want `events.Event without DedupeValues`
	}
}

package model

import (
	"context"
	"sync"
)

// Fake is a Runner for tests: canned responses, recorded inputs.
type Fake struct {
	// Response answers every call; Responses, when set, answers by call index.
	Response  string
	Responses []string
	// Usage is reported per call.
	Usage Usage
	Err   error
	Calls []FakeCall
	mu    sync.Mutex
}

type FakeCall struct {
	Model  string
	Effort string
	Input  string
}

func (f *Fake) Run(ctx context.Context, call Call, input string) (string, Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Model: call.Model, Effort: call.Effort, Input: input})
	if f.Err != nil {
		return "", Usage{}, f.Err
	}
	if f.Responses != nil {
		if len(f.Calls) <= len(f.Responses) {
			return f.Responses[len(f.Calls)-1], f.Usage, nil
		}
		return f.Responses[len(f.Responses)-1], f.Usage, nil
	}
	return f.Response, f.Usage, nil
}

// FakeEditor is an Editor for tests: Apply stands in for the model's edits,
// and its return is the final text.
type FakeEditor struct {
	Apply func(input string, dirs []string) (string, error)
	Usage Usage
	Calls []FakeEdit
	mu    sync.Mutex
}

type FakeEdit struct {
	Model  string
	Effort string
	Input  string
	Dirs   []string
}

func (f *FakeEditor) Edit(ctx context.Context, call Call, input string, dirs []string) (string, Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeEdit{Model: call.Model, Effort: call.Effort, Input: input, Dirs: dirs})
	if f.Apply == nil {
		return "", f.Usage, nil
	}
	out, err := f.Apply(input, dirs)
	if err != nil {
		return "", Usage{}, err
	}
	return out, f.Usage, nil
}

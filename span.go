// Copyright (C) 2015 Space Monkey, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package monitor

import (
	"fmt"
	"sort"
	"time"

	"github.com/spacemonkeygo/monotime"
	"golang.org/x/net/context"
)

type ctxKey int

const (
	spanKey ctxKey = iota
)

type Annotation struct {
	Name  string
	Value string
}

type Span struct {
	// sync/atomic things
	mtx spinLock

	// immutable things from construction
	id     int64
	start  time.Time
	f      *Func
	trace  *Trace
	parent *Span
	args   []interface{}
	context.Context

	// protected by mtx
	done        bool
	orphaned    bool
	children    spanBag
	annotations []Annotation
}

func SpanFromCtx(ctx context.Context) *Span {
	if s, ok := ctx.(*Span); ok && s != nil {
		return s
	} else if s, ok := ctx.Value(spanKey).(*Span); ok && s != nil {
		return s
	}
	return nil
}

func newSpan(ctx context.Context, f *Func, args []interface{},
	id int64, trace *Trace) (s *Span, exit func(*error)) {

	var parent *Span
	if s, ok := ctx.(*Span); ok && s != nil {
		ctx = s.Context
		if trace == nil {
			parent = s
			trace = parent.trace
		}
	} else if s, ok := ctx.Value(spanKey).(*Span); ok && s != nil {
		if trace == nil {
			parent = s
			trace = parent.trace
		}
	} else if trace == nil {
		trace = NewTrace(id)
		f.scope.r.observeTrace(trace)
	}

	observer := trace.getObserver()

	s = &Span{
		id:      id,
		start:   monotime.Now(),
		f:       f,
		trace:   trace,
		parent:  parent,
		args:    args,
		Context: ctx}

	if parent != nil {
		f.start(parent.f)
		parent.addChild(s)
	} else {
		f.start(nil)
		f.scope.r.rootSpanStart(s)
	}

	if observer != nil {
		observer.Start(s)
	}

	return s, func(errptr *error) {
		rec := recover()
		panicked := rec != nil

		finish := monotime.Now()

		var err error
		if errptr != nil {
			err = *errptr
		}
		s.f.end(err, panicked, finish.Sub(s.start))

		var children []*Span
		s.mtx.Lock()
		s.done = true
		orphaned := s.orphaned
		s.children.Iterate(func(child *Span) {
			children = append(children, child)
		})
		s.mtx.Unlock()
		for _, child := range children {
			child.orphan()
		}

		if s.parent != nil {
			s.parent.removeChild(s)
			if orphaned {
				s.f.scope.r.orphanEnd(s)
			}
		} else {
			s.f.scope.r.rootSpanEnd(s)
		}

		if observer != nil {
			observer.Finish(s, err, panicked, finish)
		}

		if panicked {
			panic(rec)
		}
	}
}

func (s *Span) addChild(child *Span) {
	s.mtx.Lock()
	s.children.Add(child)
	done := s.done
	s.mtx.Unlock()
	if done {
		child.orphan()
	}
}

func (s *Span) removeChild(child *Span) {
	s.mtx.Lock()
	s.children.Remove(child)
	s.mtx.Unlock()
}

func (s *Span) orphan() {
	s.mtx.Lock()
	if !s.done && !s.orphaned {
		s.orphaned = true
		s.f.scope.r.orphanedSpan(s)
	}
	s.mtx.Unlock()
}

func (s *Span) Duration() time.Duration {
	return monotime.Now().Sub(s.start)
}

func (s *Span) Start() time.Time {
	return s.start
}

func (s *Span) Value(key interface{}) interface{} {
	if key == spanKey {
		return s
	}
	return s.Context.Value(key)
}

func (s *Span) String() string {
	// TODO: for working with Contexts
	return fmt.Sprintf("%v.WithSpan()", s.Context)
}

func (s *Span) Children(cb func(s *Span)) {
	found := map[*Span]bool{}
	var sorter []*Span
	s.mtx.Lock()
	s.children.Iterate(func(s *Span) {
		if !found[s] {
			found[s] = true
			sorter = append(sorter, s)
		}
	})
	s.mtx.Unlock()
	sort.Sort(spanSorter(sorter))
	for _, s := range sorter {
		cb(s)
	}
}

func (s *Span) Args() (rv []string) {
	rv = make([]string, 0, len(s.args))
	for _, arg := range s.args {
		rv = append(rv, fmt.Sprintf("%#v", arg))
	}
	return rv
}

func (s *Span) Id() int64     { return s.id }
func (s *Span) Func() *Func   { return s.f }
func (s *Span) Trace() *Trace { return s.trace }
func (s *Span) Parent() *Span { return s.parent }

func (s *Span) Annotations() []Annotation {
	s.mtx.Lock()
	annotations := s.annotations // okay cause we only ever append to this slice
	s.mtx.Unlock()
	return append([]Annotation(nil), annotations...)
}

func (s *Span) Annotate(name, val string) {
	s.mtx.Lock()
	s.annotations = append(s.annotations, Annotation{Name: name, Value: val})
	s.mtx.Unlock()
}

func (s *Span) Orphaned() (rv bool) {
	s.mtx.Lock()
	rv = s.orphaned
	s.mtx.Unlock()
	return rv
}

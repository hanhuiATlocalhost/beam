// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exec

import (
	"context"
	"fmt"
	"io"
	"path"
	"reflect"

	"github.com/apache/beam/sdks/go/pkg/beam/core/funcx"
	"github.com/apache/beam/sdks/go/pkg/beam/core/graph"
	"github.com/apache/beam/sdks/go/pkg/beam/core/typex"
	"github.com/apache/beam/sdks/go/pkg/beam/core/util/reflectx"
	"github.com/apache/beam/sdks/go/pkg/beam/util/errorx"
)

// Combine is a Combine executor. Combiners do not have side inputs (or output).
type Combine struct {
	UID     UnitID
	Fn      *graph.CombineFn
	UsesKey bool
	Out     Node

	mergeFn reflectx.Func2x1 // optimized caller in the case of binary merge accumulators

	status Status
	err    errorx.GuardedError
}

func (n *Combine) ID() UnitID {
	return n.UID
}

func (n *Combine) Up(ctx context.Context) error {
	if n.status != Initializing {
		return fmt.Errorf("invalid status for combine %v: %v", n.UID, n.status)
	}
	n.status = Up

	if _, err := InvokeWithoutEventTime(ctx, n.Fn.SetupFn(), nil); err != nil {
		return n.fail(err)
	}

	if n.Fn.AddInputFn() == nil {
		n.mergeFn = reflectx.ToFunc2x1(n.Fn.MergeAccumulatorsFn().Fn)
	}
	return nil
}

func (n *Combine) StartBundle(ctx context.Context, id string, data DataManager) error {
	if n.status != Up {
		return fmt.Errorf("invalid status for combine %v: %v", n.UID, n.status)
	}
	n.status = Active

	if err := n.Out.StartBundle(ctx, id, data); err != nil {
		return n.fail(err)
	}
	return nil
}

func (n *Combine) ProcessElement(ctx context.Context, value FullValue, values ...ReStream) error {
	if n.status != Active {
		return fmt.Errorf("invalid status for combine %v: %v", n.UID, n.status)
	}

	// Note that we do not explicitly call merge, although it may
	// be called implicitly when adding input.

	a, err := n.newAccum(ctx, value.Elm)
	if err != nil {
		return n.fail(err)
	}
	first := true

	stream := values[0].Open()
	defer stream.Close()
	for {
		v, err := stream.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return n.fail(err)
		}

		a, err = n.addInput(ctx, a, value.Elm, v.Elm, value.Timestamp, first)
		if err != nil {
			return n.fail(err)
		}
		first = false
	}

	out, err := n.extract(ctx, a)
	if err != nil {
		return n.fail(err)
	}
	return n.Out.ProcessElement(ctx, FullValue{Windows: value.Windows, Elm: value.Elm, Elm2: out, Timestamp: value.Timestamp})
}

func (n *Combine) FinishBundle(ctx context.Context) error {
	if n.status != Active {
		return fmt.Errorf("invalid status for combine %v: %v", n.UID, n.status)
	}
	n.status = Up

	if err := n.Out.FinishBundle(ctx); err != nil {
		return n.fail(err)
	}
	return nil
}

func (n *Combine) Down(ctx context.Context) error {
	if n.status == Down {
		return n.err.Error()
	}
	n.status = Down

	if _, err := InvokeWithoutEventTime(ctx, n.Fn.TeardownFn(), nil); err != nil {
		n.err.TrySetError(err)
	}
	return n.err.Error()
}

func (n *Combine) newAccum(ctx context.Context, key interface{}) (interface{}, error) {
	fn := n.Fn.CreateAccumulatorFn()
	if fn == nil {
		return reflect.Zero(n.Fn.MergeAccumulatorsFn().Ret[0].T).Interface(), nil
	}

	var opt *MainInput
	if n.UsesKey {
		opt = &MainInput{Key: FullValue{Elm: key}}
	}

	val, err := InvokeWithoutEventTime(ctx, fn, opt)
	if err != nil {
		return nil, fmt.Errorf("CreateAccumulator failed: %v", err)
	}
	return val.Elm, nil
}

func (n *Combine) addInput(ctx context.Context, accum, key, value interface{}, timestamp typex.EventTime, first bool) (interface{}, error) {
	// log.Printf("AddInput: %v %v into %v", key, value, accum)

	fn := n.Fn.AddInputFn()
	if fn == nil {
		// Merge function only. The input value is an accumulator. We only do a binary
		// merge if we've actually seen a value.
		if first {
			return value, nil
		}

		// TODO(herohde) 7/5/2017: do we want to allow addInput to be optional
		// if non-binary merge is defined?

		return n.mergeFn.Call2x1(accum, value), nil
	}

	opt := &MainInput{
		Key: FullValue{
			Elm:       accum,
			Timestamp: timestamp,
		},
	}

	in := fn.Params(funcx.FnValue | funcx.FnIter | funcx.FnReIter)
	i := 1
	if n.UsesKey {
		opt.Key.Elm2 = Convert(key, fn.Param[in[i]].T)
		i++
	}
	v := Convert(value, fn.Param[i].T)

	val, err := InvokeWithoutEventTime(ctx, n.Fn.AddInputFn(), opt, v)
	if err != nil {
		return nil, n.fail(fmt.Errorf("AddInput failed: %v", err))
	}
	return val.Elm, err
}

func (n *Combine) extract(ctx context.Context, accum interface{}) (interface{}, error) {
	fn := n.Fn.ExtractOutputFn()
	if fn == nil {
		// Merge function only. Accumulator type is the output type.
		return accum, nil
	}

	val, err := InvokeWithoutEventTime(ctx, n.Fn.ExtractOutputFn(), nil, accum)
	if err != nil {
		return nil, n.fail(fmt.Errorf("ExtractOutput failed: %v", err))
	}
	return val.Elm, err
}

func (n *Combine) fail(err error) error {
	n.status = Broken
	n.err.TrySetError(err)
	return err
}

func (n *Combine) String() string {
	return fmt.Sprintf("Combine[%v] Keyed:%v Out:%v", path.Base(n.Fn.Name()), n.UsesKey, n.Out.ID())
}

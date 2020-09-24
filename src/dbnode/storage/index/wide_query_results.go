// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package index

import (
	"github.com/m3db/m3/src/m3ninx/doc"
	"github.com/m3db/m3/src/x/ident"
)

type wideResults struct {
	nsID   ident.ID
	opts   QueryResultsOptions
	idPool ident.Pool

	closed      bool
	idsOverflow []ident.ID
	batch       *ident.IDBatch
	batchCh     chan<- *ident.IDBatch
	batchSize   int
}

// NewWideQueryResults returns a new wide query results object.
// NB: Reader must read results from `batchCh` in a goroutine, and call
// batch.Done() after the result is used, and the writer must close the
// channel after no more Documents are available.
func NewWideQueryResults(
	namespaceID ident.ID,
	batchSize int,
	idPool ident.Pool,
	batchCh chan<- *ident.IDBatch,
	opts QueryResultsOptions,
) BaseResults {
	return &wideResults{
		nsID:        namespaceID,
		idPool:      idPool,
		batchSize:   batchSize,
		idsOverflow: make([]ident.ID, 0, batchSize),
		batch: &ident.IDBatch{
			IDs: make([]ident.ID, 0, batchSize),
		},
		batchCh: batchCh,
		opts:    opts,
	}
}

func (r *wideResults) AddDocuments(batch []doc.Document) (int, int, error) {
	if r.closed {
		return 0, 0, nil
	}

	err := r.addDocumentsBatchWithLock(batch)
	release := len(r.batch.IDs) >= r.batchSize
	// fmt.Println("release", release, len(r.ids), r.batchSize)
	// fmt.Println(r.ids)
	if release {
		// fmt.Println("released", r.ids)
		r.releaseAndWait()
		r.releaseOverflow(false)
	}

	return 0, 0, err
}

func (r *wideResults) releaseOverflow(forceRelease bool) {
	var (
		incomplete bool
		size       int
		overflow   int
	)
	for {
		size = r.batchSize
		overflow = len(r.idsOverflow)
		if overflow == 0 {
			// NB: no overflow elements.
			return
		}

		if overflow < size {
			size = overflow
			incomplete = true
		}

		// fmt.Println("batch overflow", r.idsOverflow)
		// fmt.Println("batch before", r.ids)
		copy(r.batch.IDs, r.idsOverflow[0:size])
		r.batch.IDs = r.batch.IDs[:size]
		// fmt.Println("batch after", r.ids)
		copy(r.idsOverflow, r.idsOverflow[size:])
		r.idsOverflow = r.idsOverflow[:overflow-size]
		// fmt.Println("batch doubleAfter", r.ids)
		// fmt.Println("batch overfloiwafter", r.idsOverflow)
		if !forceRelease && incomplete {
			return
		}

		r.releaseAndWait()
	}
}

func (r *wideResults) addDocumentsBatchWithLock(batch []doc.Document) error {
	for i := range batch {
		err := r.addDocumentWithLock(batch[i])
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *wideResults) addDocumentWithLock(d doc.Document) error {
	if len(d.ID) == 0 {
		return errUnableToAddResultMissingID
	}

	var tsID ident.ID = ident.BytesID(d.ID)

	// Need to apply filter if set first.
	if r.opts.FilterID != nil && !r.opts.FilterID(tsID) {
		return nil
	}

	// Pool IDs after filter is passed.
	tsID = r.idPool.Clone(tsID)
	if len(r.batch.IDs) < r.batchSize {
		r.batch.IDs = append(r.batch.IDs, tsID)
	} else {
		r.idsOverflow = append(r.idsOverflow, tsID)
	}

	return nil
}

func (r *wideResults) Namespace() ident.ID {
	return r.nsID
}

func (r *wideResults) Size() int {
	return 0
}

func (r *wideResults) TotalDocsCount() int {
	return 0
}

// NB: Finalize should be called after all documents have been consumed.
func (r *wideResults) Finalize() {
	if r.closed {
		return
	}

	r.closed = true
	r.releaseAndWait()
	r.releaseOverflow(true)
	close(r.batchCh)
}

func (r *wideResults) releaseAndWait() {
	if r.closed {
		return
	}

	r.batch.Add(1)
	r.batchCh <- r.batch
	r.batch.Wait()
}

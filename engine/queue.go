package engine

import (
	"context"
	"errors"
	"time"

	"github.com/sandgorgon/polydb/keys"
	"github.com/sandgorgon/polydb/model"
	"github.com/sandgorgon/polydb/value"
)

// queueOf returns a queue field of st, its current queue value and the
// queue's type. An absent or null queue field is an empty queue.
func (tx *Tx) queueOf(st *entState, name string) (*model.Field, value.Queue, *value.Type, error) {
	f, ok := st.fields[name]
	var fd *model.FieldDef
	if ok {
		fd = fieldDef(st.kind, f)
	} else if st.kind != nil {
		fd, _ = st.kind.Field(name)
	}
	if fd == nil {
		return nil, value.Queue{}, nil, &Error{Code: CodeUnknownField, Msg: model.UnknownField(name, st.names()).Error()}
	}
	var cur value.Value
	if ok {
		cur = f.Value
	}
	if q, isQ := cur.(value.Queue); isQ {
		t := fd.Type
		if !t.Code.IsQueue() { // a queue held in an any or union field
			t = &value.Type{Code: q.Code(), Elem: value.TypeAny, N: q.Limit(), LowFirst: q.LowFirst()}
		}
		return f, q, t, nil
	}
	if !fd.Type.Code.IsQueue() {
		return nil, value.Queue{}, nil, errorf(CodeType, "field %s is %s, not a queue", name, fd.Type)
	}
	q, err := value.NewQueue(fd.Type)
	if err != nil {
		return nil, value.Queue{}, nil, wrap(CodeInternal, err)
	}
	if !ok {
		f = &model.Field{ID: tx.newID(), Entity: st.e.ID, Name: name, Declared: true}
	}
	return f, q, fd.Type, nil
}

func (st *entState) putValue(f *model.Field, v value.Value) {
	cp := *f
	cp.Value = v
	st.fields[f.Name] = &cp
}

// Push adds v to a queue field (spec/rhizome.md §6.5). priority is used by
// priority queues only. Pushing to a full fifo, lifo or priority queue
// fails; a full ring drops its oldest element.
func (tx *Tx) Push(id value.ID, field string, v value.Value, priority int64) error {
	st, err := tx.loadEntity(id)
	if err != nil {
		return err
	}
	f, q, t, err := tx.queueOf(st, field)
	if err != nil {
		return err
	}
	c, err := tx.conform(&model.FieldDef{Name: field, Type: t.Elem}, v)
	if err != nil {
		return err
	}
	q, err = q.Push(c, priority)
	if errors.Is(err, value.ErrQueueFull) {
		return errorf(CodeConstraint, "queue %s is full (max %d)", field, q.Limit())
	} else if err != nil {
		return errorf(CodeLimit, "queue %s: %v", field, err)
	}
	st.putValue(f, q)
	return tx.fail2(tx.save(st))
}

// Pop removes and returns up to n elements from a queue field, in pop
// order. An empty queue gives an empty slice.
func (tx *Tx) Pop(id value.ID, field string, n int) ([]value.Value, error) {
	st, err := tx.loadEntity(id)
	if err != nil {
		return nil, err
	}
	f, q, _, err := tx.queueOf(st, field)
	if err != nil {
		return nil, err
	}
	var out []value.Value
	for len(out) < n && q.Len() > 0 {
		var v value.Value
		q, v = q.Pop()
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	st.putValue(f, q)
	return out, tx.fail2(tx.save(st))
}

// PopOne removes and returns the next element, or null if the queue is
// empty.
func (tx *Tx) PopOne(id value.ID, field string) (value.Value, error) {
	vs, err := tx.Pop(id, field, 1)
	if err != nil || len(vs) == 0 {
		return value.Null, err
	}
	return vs[0], nil
}

// Queue returns the current value of a queue field without changing it,
// for peek, len and reading in pop order.
func (tx *Tx) Queue(id value.ID, field string) (value.Queue, error) {
	st, err := tx.loadEntity(id)
	if err != nil {
		return value.Queue{}, err
	}
	_, q, _, err := tx.queueOf(st, field)
	return q, err
}

// --- pop … wait ---

// waiter is one PopWait call waiting on a queue.
type waiter struct {
	ch     chan struct{} // buffered; a signal means "try popping"
	closed bool          // the database was closed
}

func (w *waiter) signal() {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// waitList holds the waiters on one queue in arrival order. Only the
// first waiter pops, so waiters are served in the order they started.
type waitList struct{ list []*waiter }

// notify is the transaction manager's commit hook: it wakes the first
// waiter on every queue field the commit wrote.
func (db *DB) notify(_ uint64, ks [][]byte) {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	if len(db.waiters) == 0 {
		return
	}
	for _, k := range ks {
		if wl := db.waiters[string(k)]; wl != nil && len(wl.list) > 0 {
			wl.list[0].signal()
		}
	}
}

// PopWait pops the next element of a queue field in its own transaction,
// waiting up to wait for one to arrive if the queue is empty, and returns
// null if none did (spec/rhizome.md §6.5, pop … wait). Waiters on the same
// queue are served in the order they started waiting. The wait is capped
// by Options.MaxPopWait and ends early if ctx is cancelled.
func (db *DB) PopWait(ctx context.Context, id value.ID, field string, wait time.Duration, opts TxOptions) (value.Value, error) {
	wait = min(wait, db.eng.opts.MaxPopWait)
	k := string(keys.Field(id, field))
	w := &waiter{ch: make(chan struct{}, 1)}

	db.wmu.Lock()
	wl := db.waiters[k]
	if wl == nil {
		wl = &waitList{}
		db.waiters[k] = wl
	}
	wl.list = append(wl.list, w)
	first := len(wl.list) == 1
	db.wmu.Unlock()
	defer db.removeWaiter(k, w)
	if first {
		w.signal()
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-w.ch:
		case <-timer.C:
			return value.Null, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		db.wmu.Lock()
		head, closed := len(wl.list) > 0 && wl.list[0] == w, w.closed
		db.wmu.Unlock()
		if closed {
			return nil, errorf(CodeInternal, "database %s was closed", db.name)
		}
		if !head {
			continue
		}
		v, ok, err := db.tryPop(id, field, opts)
		if err != nil || ok {
			return v, err
		}
	}
}

// tryPop pops one element in its own transaction, retrying conflicts.
func (db *DB) tryPop(id value.ID, field string, opts TxOptions) (value.Value, bool, error) {
	for {
		tx := db.Begin(opts)
		vs, err := tx.Pop(id, field, 1)
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if IsConflict(err) {
			continue
		}
		if err != nil || len(vs) == 0 {
			return value.Null, false, err
		}
		return vs[0], true, nil
	}
}

// removeWaiter takes w off its list and lets the next waiter try, since
// the queue may still hold elements.
func (db *DB) removeWaiter(k string, w *waiter) {
	db.wmu.Lock()
	defer db.wmu.Unlock()
	wl := db.waiters[k]
	if wl == nil {
		return
	}
	for i, x := range wl.list {
		if x == w {
			wl.list = append(wl.list[:i], wl.list[i+1:]...)
			break
		}
	}
	if len(wl.list) == 0 {
		delete(db.waiters, k)
		return
	}
	wl.list[0].signal()
}

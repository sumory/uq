package queue

import (
	"bytes"
	"container/list"
	"encoding/gob"
	"log"
	"sync"
	"time"

	"github.com/buaazp/uq/utils"
)

func init() {
	gob.Register(&lineStore{})
}

type line struct {
	name         string
	head         uint64
	headLock     sync.RWMutex
	recycle      time.Duration
	recycleKey   string
	inflight     *list.List
	inflightLock sync.RWMutex
	ihead        uint64
	imap         map[uint64]bool
	t            *topic
}

type lineStore struct {
	Head      uint64
	Inflights []inflightMessage
	Ihead     uint64
}

func (l *line) exportRecycle() error {
	lineRecycleData := []byte(l.recycle.String())
	err := l.t.q.setData(l.recycleKey, lineRecycleData)
	if err != nil {
		return err
	}
	return nil
}

func (l *line) removeRecycleData() error {
	err := l.t.q.delData(l.recycleKey)
	if err != nil {
		return err
	}
	return nil
}

func (l *line) genLineStore() *lineStore {
	inflights := make([]inflightMessage, l.inflight.Len())
	i := 0
	for m := l.inflight.Front(); m != nil; m = m.Next() {
		msg := m.Value.(*inflightMessage)
		inflights[i] = *msg
		i++
	}
	// log.Printf("inflights: %v", inflights)

	ls := new(lineStore)
	ls.Head = l.head
	ls.Inflights = inflights
	ls.Ihead = l.ihead
	return ls
}

func (l *line) exportLine() error {
	// log.Printf("start export line[%s]...", l.name)
	lineStoreValue := l.genLineStore()

	buffer := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(buffer)
	err := enc.Encode(lineStoreValue)
	if err != nil {
		return err
	}

	lineStoreKey := l.t.name + "/" + l.name
	err = l.t.q.setData(lineStoreKey, buffer.Bytes())
	if err != nil {
		return err
	}

	// log.Printf("line[%s] export finisded.", l.name)
	return nil
}

func (l *line) removeLineData() error {
	lineStoreKey := l.t.name + "/" + l.name
	err := l.t.q.delData(lineStoreKey)
	if err != nil {
		return err
	}

	// log.Printf("line[%s] remove finisded.", l.name)
	return nil
}

func (l *line) updateiHead() {
	for l.ihead < l.head {
		id := l.ihead
		fl, ok := l.imap[id]
		if !ok {
			l.ihead++
			continue
		}
		if fl {
			return
		}
		delete(l.imap, id)
		l.ihead++
	}
}

func (l *line) pop() (uint64, []byte, error) {
	l.inflightLock.Lock()
	defer l.inflightLock.Unlock()

	now := time.Now()
	if l.recycle > 0 {

		m := l.inflight.Front()
		if m != nil {
			msg := m.Value.(*inflightMessage)
			if now.After(msg.Exptime) {
				// log.Printf("key[%s/%d] is expired.", l.name, msg.Tid)
				msg.Exptime = now.Add(l.recycle)
				data, err := l.t.getData(msg.Tid)
				if err != nil {
					return 0, nil, err
				}
				l.inflight.Remove(m)
				l.inflight.PushBack(msg)
				// log.Printf("key[%s/%s/%d] poped.", l.t.name, l.name, msg.Tid)
				return msg.Tid, data, nil
			}
		}
	}

	l.headLock.Lock()
	defer l.headLock.Unlock()
	tid := l.head

	topicTail := l.t.getTail()
	if l.head >= topicTail {
		// log.Printf("line[%s] is blank. head:%d - tail:%d", l.name, l.head, l.t.tail)
		return 0, nil, utils.NewError(
			utils.ErrNone,
			`line pop`,
		)
	}

	data, err := l.t.getData(tid)
	if err != nil {
		return 0, nil, err
	}

	l.head++

	if l.recycle > 0 {
		msg := new(inflightMessage)
		msg.Tid = tid
		msg.Exptime = now.Add(l.recycle)

		l.inflight.PushBack(msg)
		// log.Printf("key[%s/%s/%d] flighted.", l.t.name, l.name, l.head)
		l.imap[tid] = true
	}

	return tid, data, nil
}

func (l *line) mPop(n int) ([]uint64, [][]byte, error) {
	l.inflightLock.Lock()
	defer l.inflightLock.Unlock()

	fc := 0
	var ids []uint64
	var datas [][]byte
	now := time.Now()
	if l.recycle > 0 {
		for m := l.inflight.Front(); m != nil && fc < n; m = m.Next() {
			msg := m.Value.(*inflightMessage)
			if now.After(msg.Exptime) {
				msg := m.Value.(*inflightMessage)
				data, err := l.t.getData(msg.Tid)
				if err != nil {
					return nil, nil, err
				}
				ids = append(ids, msg.Tid)
				datas = append(datas, data)
				fc++
			} else {
				break
			}
		}
		exptime := now.Add(l.recycle)
		for i := 0; i < fc; i++ {
			m := l.inflight.Front()
			msg := m.Value.(*inflightMessage)
			msg.Exptime = exptime
			l.inflight.Remove(m)
			l.inflight.PushBack(msg)
		}
		if fc >= n {
			return ids, datas, nil
		}
	}

	l.headLock.Lock()
	defer l.headLock.Unlock()

	for ; fc < n; fc++ {
		tid := l.head
		topicTail := l.t.getTail()
		if l.head >= topicTail {
			// log.Printf("line[%s] is blank. head:%d - tail:%d", l.name, l.head, l.t.tail)
			break
		}

		data, err := l.t.getData(tid)
		if err != nil {
			log.Printf("get data failed: %s", err)
			break
		}

		l.head++
		ids = append(ids, tid)
		datas = append(datas, data)

		if l.recycle > 0 {
			msg := new(inflightMessage)
			msg.Tid = tid
			msg.Exptime = now.Add(l.recycle)

			l.inflight.PushBack(msg)
			// log.Printf("key[%s/%s/%d] flighted.", l.t.name, l.name, l.head)
			l.imap[tid] = true
		}
	}

	if len(ids) > 0 {
		return ids, datas, nil
	}
	return nil, nil, utils.NewError(
		utils.ErrNone,
		`line mPop`,
	)
}

func (l *line) confirm(id uint64) error {
	if l.recycle == 0 {
		return utils.NewError(
			utils.ErrNotDelivered,
			`line confirm`,
		)
	}

	l.headLock.RLock()
	defer l.headLock.RUnlock()
	head := l.head
	if id >= head {
		return utils.NewError(
			utils.ErrNotDelivered,
			`line confirm`,
		)
	}

	l.inflightLock.Lock()
	defer l.inflightLock.Unlock()

	for m := l.inflight.Front(); m != nil; m = m.Next() {
		msg := m.Value.(*inflightMessage)
		if msg.Tid == id {
			l.inflight.Remove(m)
			// log.Printf("key[%s/%s/%d] comfirmed.", l.t.name, l.name, id)
			l.imap[id] = false
			l.updateiHead()
			return nil
		}
	}

	return utils.NewError(
		utils.ErrNotDelivered,
		`line confirm`,
	)
}

func (l *line) stat() *Stat {
	l.inflightLock.RLock()
	defer l.inflightLock.RUnlock()
	l.headLock.RLock()
	defer l.headLock.RUnlock()

	qs := new(Stat)
	qs.Name = l.t.name + "/" + l.name
	qs.Type = "line"
	qs.Recycle = l.recycle.String()
	qs.IHead = l.ihead
	inflightLen := uint64(l.inflight.Len())
	qs.Head = l.head
	qs.Tail = l.t.getTail()
	qs.Count = inflightLen + qs.Tail - qs.Head

	return qs
}

func (l *line) empty() error {
	l.inflightLock.Lock()
	defer l.inflightLock.Unlock()
	l.inflight.Init()
	l.imap = make(map[uint64]bool)
	l.ihead = l.t.getTail()

	l.headLock.Lock()
	defer l.headLock.Unlock()
	l.head = l.t.getTail()

	err := l.exportLine()
	if err != nil {
		return err
	}

	log.Printf("line[%s] empty succ", l.name)
	return nil
}

func (l *line) remove() error {
	err := l.removeLineData()
	if err != nil {
		log.Printf("line[%s] removeLineData error: %s", l.name, err)
	}

	err = l.removeRecycleData()
	if err != nil {
		log.Printf("line[%s] removeRecycleData error: %s", l.name, err)
	}

	log.Printf("line[%s] remove succ", l.name)
	return nil
}

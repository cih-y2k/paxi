package async_paxos

import (
	"math/rand"
	"time"

	"github.com/ailidani/paxi"
	"github.com/ailidani/paxi/log"
)

// entry in log
type entry struct {
	ballot    paxi.Ballot
	command   paxi.Command
	commit    bool
	request   *paxi.Request
	quorum    *paxi.Quorum
	timestamp time.Time
}

// Paxos instance
type Paxos struct {
	paxi.Node

	log     map[int]*entry // log ordered by slot
	execute int            // next execute slot number
	active  bool           // active leader
	ballot  paxi.Ballot    // highest ballot number
	slot    int            // highest slot number

	quorum   *paxi.Quorum    // phase 1 quorum
	requests []*paxi.Request // phase 1 pending requests
	sleeping bool
}

// NewPaxos creates new paxos instance
func NewPaxos(n paxi.Node) *Paxos {
	log := make(map[int]*entry, paxi.BUFFER_SIZE)
	log[0] = &entry{}
	return &Paxos{
		Node:     n,
		log:      log,
		execute:  1,
		quorum:   paxi.NewQuorum(),
		requests: make([]*paxi.Request, 0),
	}
}

// IsLeader indecates if this node is current leader
func (p *Paxos) IsLeader() bool {
	return p.active || p.ballot.ID() == p.ID()
}

// Leader returns leader id of the current ballot
func (p *Paxos) Leader() paxi.ID {
	return p.ballot.ID()
}

// Ballot returns current ballot
func (p *Paxos) Ballot() paxi.Ballot {
	return p.ballot
}

// HandleRequest handles request and start phase 1 or phase 2
func (p *Paxos) HandleRequest(r paxi.Request) {
	log.Debugf("Replica %s received %v\n", p.ID(), r)
	if !p.active {
		p.requests = append(p.requests, &r)
		// current phase 1 pending
		if p.ballot.ID() != p.ID() {
			p.P1a()
		}
	} else {
		p.P2a(&r)
	}
}

// P1a starts phase 1 prepare
func (p *Paxos) P1a() {
	if p.active {
		return
	}
	p.ballot.Next(p.ID())
	p.quorum.Reset()
	p.quorum.ACK(p.ID())
	m := P1a{Ballot: p.ballot}
	log.Debugf("Replica %s broadcast [%v]\n", p.ID(), m)
	p.Broadcast(&m)
}

// P2a starts phase 2 accept
func (p *Paxos) P2a(r *paxi.Request) {
	p.slot++
	p.log[p.slot] = &entry{
		ballot:    p.ballot,
		command:   r.Command,
		commit:    true,
		request:   r,
		quorum:    paxi.NewQuorum(),
		timestamp: time.Now(),
	}
	p.log[p.slot].quorum.ACK(p.ID())
	m := P2a{
		Ballot:  p.ballot,
		Slot:    p.slot,
		Command: r.Command,
	}
	log.Debugf("Replica %s broadcast [%v]\n", p.ID(), m)
	p.Broadcast(&m)
	// execute and reply
	p.exec()
}

func (p *Paxos) HandleP1a(m P1a) {
	// new leader
	if m.Ballot > p.ballot {
		p.ballot = m.Ballot
		p.active = false
		if len(p.requests) > 0 {
			p.sleeping = true
			go func() {
				time.Sleep(time.Millisecond * time.Duration(rand.Intn(100)+p.Config().BackOff))
				p.P1a()
				p.sleeping = false
			}()
		}
	}

	l := make(map[int]CommandBallot)
	for s := p.execute; s <= p.slot; s++ {
		if p.log[s] == nil || p.log[s].commit {
			continue
		}
		l[s] = CommandBallot{p.log[s].command, p.log[s].ballot}
	}

	p.Send(m.Ballot.ID(), &P1b{
		Ballot: p.ballot,
		ID:     p.ID(),
		Log:    l,
	})
}

func (p *Paxos) update(scb map[int]CommandBallot) {
	for s, cb := range scb {
		p.slot = paxi.Max(p.slot, s)
		if e, exists := p.log[s]; exists {
			if !e.commit && cb.Ballot > e.ballot {
				e.ballot = cb.Ballot
				e.command = cb.Command
			}
		} else {
			p.log[s] = &entry{
				ballot:  cb.Ballot,
				command: cb.Command,
				commit:  false,
			}
		}
	}
}

// HandleP1b handles p1b message
func (p *Paxos) HandleP1b(m P1b) {
	// old message
	if m.Ballot < p.ballot || p.active {
		// log.Debugf("Replica %s ignores old message [%v]\n", p.ID(), m)
		return
	}

	log.Debugf("Replica %s ===[%v]===>>> Replica %s\n", m.ID, m, p.ID())

	p.update(m.Log)

	// reject message
	if m.Ballot > p.ballot {
		p.ballot = m.Ballot
		p.active = false // not necessary
		if !p.sleeping {
			p.sleeping = true
			go func() {
				time.Sleep(time.Millisecond * time.Duration(rand.Intn(100)+p.Config().BackOff))
				p.P1a()
				p.sleeping = false
			}()
		}
	}

	// ack message
	if m.Ballot.ID() == p.ID() && m.Ballot == p.ballot {
		p.quorum.ACK(m.ID)
		if p.quorum.Q1() {
			p.active = true
			// propose any uncommitted entries
			for i := p.execute; i <= p.slot; i++ {
				// TODO nil gap?
				if p.log[i] == nil || p.log[i].commit {
					continue
				}
				p.log[i].commit = true
				p.log[i].ballot = p.ballot
				p.log[i].quorum = paxi.NewQuorum()
				p.log[i].quorum.ACK(p.ID())
				m := P2a{
					Ballot:  p.ballot,
					Slot:    i,
					Command: p.log[i].command,
				}
				log.Debugf("Replica %s broadcast [%v]\n", p.ID(), m)
				p.Broadcast(&m)
			}
			// propose new commands
			for _, req := range p.requests {
				p.P2a(req)
			}
			p.requests = make([]*paxi.Request, 0)
		}
	}
}

func (p *Paxos) HandleP2a(m P2a) {
	// log.Debugf("Replica %s ===[%v]===>>> Replica %s\n", m.Ballot.ID(), m, p.ID())

	if m.Ballot >= p.ballot {
		p.ballot = m.Ballot
		p.active = false
		// update slot number
		p.slot = paxi.Max(p.slot, m.Slot)
		// update entry
		if e, exists := p.log[m.Slot]; exists {
			if !e.commit && m.Ballot > e.ballot {
				// different command and request is not nil
				if !e.command.Equal(m.Command) && e.request != nil {
					p.Retry(*e.request)
					e.request = nil
				}
				e.command = m.Command
				e.ballot = m.Ballot
				e.commit = true
			}
		} else {
			p.log[m.Slot] = &entry{
				ballot:  m.Ballot,
				command: m.Command,
				commit:  true,
			}
		}
	}

	p.Send(m.Ballot.ID(), &P2b{
		Ballot: p.ballot,
		Slot:   m.Slot,
		ID:     p.ID(),
	})
}

func (p *Paxos) HandleP2b(m P2b) {
	// old message
	if m.Ballot < p.log[m.Slot].ballot || p.log[m.Slot].commit {
		return
	}

	log.Debugf("Replica %s ===[%v]===>>> Replica %s\n", m.ID, m, p.ID())

	// reject message
	// node update its ballot number and falls back to acceptor
	if m.Ballot > p.ballot {
		p.ballot = m.Ballot
		p.active = false
	}

	// ack message
	// the current slot might still be committed with q2
	// if no q2 can be formed, this slot will be retried when received p2a or p3
	if m.Ballot.ID() == p.ID() && m.Ballot == p.log[m.Slot].ballot {
		p.log[m.Slot].quorum.ACK(m.ID)
		if p.log[m.Slot].quorum.Q2() {
			p.log[m.Slot].commit = true
		}
	}
}

func (p *Paxos) exec() {
	for {
		e, ok := p.log[p.execute]
		if !ok || !e.commit {
			break
		}

		log.Debugf("Replica %s execute [s=%d, cmd=%v]\n", p.ID(), p.execute, e.command)
		value := p.Execute(e.command)
		p.execute++

		if e.request != nil {
			e.request.Reply(paxi.Reply{
				Command: e.command,
				Value:   value,
			})
			e.request = nil
		}
	}
}

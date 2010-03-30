// Handles a peer
// Roger Pau Monné - 2010
// Distributed under the terms of the GNU GPLv3

package main

import(
	"log"
	"os"
	"net"
	"time"
	"encoding/binary"
	)

type Peer struct {
	addr, remote_peerId, our_peerId, infohash string
	numPieces int64
	wire *Wire
	bitfield *Bitfield
	our_bitfield *Bitfield
	in chan message
	incoming chan message // Exclusive channel, where peer receives messages and PeerMgr sends
	outgoing chan message // Shared channel, peer sends messages and PeerMgr receives
	requests chan PieceRequest // Shared channel with the PieceMgr, used to request new pieces
	pieces chan Request // Shared channel with PieceMgr, used to send peices we receive from peers
	delete chan message
	am_choking bool
	am_interested bool
	peer_choking bool
	peer_interested bool
	received_keepalive int64
	writeQueue *PeerQueue
}

func NewPeer(addr, infohash, peerId string, outgoing chan message, numPieces int64, requests chan PieceRequest, pieces chan Request, our_bitfield *Bitfield) (p *Peer, err os.Error) {
	p = new(Peer)
	p.addr = addr
	p.infohash = infohash
	p.our_peerId = peerId
	p.incoming = make(chan message)
	p.in = make(chan message)
	p.outgoing = outgoing
	p.am_choking = true
	p.am_interested = false
	p.peer_choking = true
	p.peer_interested = false
	p.bitfield = NewBitfield(int(numPieces))
	p.our_bitfield = our_bitfield
	p.numPieces = numPieces
	p.requests = requests
	p.pieces = pieces
	p.delete = make(chan message)
	// Start writting queue
	p.in = make(chan message)
	p.writeQueue = NewQueue(p.incoming, p.in, p.delete)
	go p.writeQueue.Run()
	return
}

func (p *Peer) PeerWriter() {
	// Create connection
	addrTCP, err := net.ResolveTCPAddr(p.addr)
	if err != nil {
		log.Stderr(err, p.addr)
		p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
		return
	}
	//log.Stderr("Connecting to", p.addr)
	conn, err := net.DialTCP("tcp4", nil, addrTCP)
	if err != nil {
		log.Stderr(err, p.addr)
		p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
		return
	}
	defer p.Close()
	err = conn.SetTimeout(TIMEOUT)
	if err != nil {
		log.Stderr(err, p.addr)
		p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
		return
	}
	// Create the wire struct
	p.wire = NewWire(p.infohash, p.our_peerId, conn)
	//log.Stderr("Sending Handshake to", p.addr)
	// Send handshake
	p.remote_peerId, err = p.wire.Handshake()
	if err != nil {
		log.Stderr(err, p.addr)
		p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
		return
	}
	// Launch peer reader
	go p.PeerReader()
	// Send the have message
	our_bitfield := p.our_bitfield.Bytes()
	//log.Stderr("Sending message:", message{length: uint32(1 + len(our_bitfield)), msgId: bitfield, payLoad: our_bitfield, addr: p.addr})
	err = p.wire.WriteMsg(message{length: uint32(1 + len(our_bitfield)), msgId: bitfield, payLoad: our_bitfield})
	if err != nil {
		log.Stderr(err, p.addr)
		p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
		return
	}
	// keep alive ticker
	keepAlive := time.Tick(KEEP_ALIVE_MSG)
	// Peer writer main bucle
	for {
		select {
			// Wait for messages or send keep-alive
			case msg := <- p.in:
				// New message to send
				err := p.wire.WriteMsg(msg)
				if err != nil {
					log.Stderr(err, p.addr)
					p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
					return
				}
				// Reset ticker
				keepAlive = time.Tick(KEEP_ALIVE_MSG)
			case <- keepAlive:
				// Send keep-alive
				//log.Stderr("Sending Keep-Alive message", p.addr)
				err := p.wire.WriteMsg(message{length: 0})
				if err != nil {
					log.Stderr(err, p.addr)
					p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
					return
				}
		}
	}
}

func (p *Peer) PeerReader() {
	defer p.Close()
	for p.wire != nil {
		msg, err := p.wire.ReadMsg()
		if err != nil {
			log.Stderr(err, p.addr)
			p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
			return
		}
		if msg.length == 0 {
			//log.Stderr("Received keep-alive from", p.addr)
			p.received_keepalive = time.Seconds()
		} else {
			err := p.ProcessMessage(*msg)
			if err != nil {
				log.Stderr(err, p.addr)
				p.outgoing <- message{length: 1, msgId: exit, addr: p.addr}
				return
			}
		}
	}
}

func (p *Peer) ProcessMessage(msg message) (err os.Error){
	switch msg.msgId {
		case choke:
			// Choke peer
			p.peer_choking = true
			//log.Stderr("Peer", p.addr, "choked")
			// If choked, clear request list
		case unchoke:
			// Unchoke peer
			p.peer_choking = false
			//log.Stderr("Peer", p.addr, "unchoked")
			// Check if we are still interested on this peer
			p.CheckInterested()
			// Notice PieceMgr of the unchoke
			p.TryToRequestPiece()
		case interested:
			// Mark peer as interested
			p.peer_interested = true
			//log.Stderr("Peer", p.addr, "interested")
		case uninterested:
			// Mark peer as uninterested
			p.peer_interested = false
			//log.Stderr("Peer", p.addr, "uninterested")
		case have:
			// Update peer bitfield
			p.bitfield.Set(int(binary.BigEndian.Uint32(msg.payLoad)))
			p.CheckInterested()
			//log.Stderr("Peer", p.addr, "have")
			// If we are unchoked notice PieceMgr of the new piece
			p.TryToRequestPiece()
		case bitfield:
			// Set peer bitfield
			//log.Stderr(msg)
			p.bitfield, err = NewBitfieldFromBytes(int(p.numPieces), msg.payLoad)
			if err != nil {
				return os.NewError("Invalid bitfield")
			}
			p.CheckInterested()
			//log.Stderr("Peer", p.addr, "bitfield")
		case request:
			// Peer requests a block
			//log.Stderr("Peer", p.addr, "requests a block")
		case piece:
			//log.Stderr("Received piece")
			p.pieces <- Request{msg: msg}
			// Check if the peer is still interesting
			p.CheckInterested()
			// Try to request another block
			p.TryToRequestPiece()
		case cancel:
			// Send the message to the sending queue to delete the "piece" message
			p.delete <- msg
		case port:
			// DHT stuff
		default:
			return os.NewError("Unknown message")
	}
	return
}

func (p *Peer) CheckInterested() {
	if p.am_interested && !p.our_bitfield.HasMorePieces(p.bitfield) {
		p.am_interested = false
		p.incoming <- message{length: 1, msgId: uninterested}
		//log.Stderr("Peer", p.addr, "marked as uninteresting")
		return
	}
	if !p.am_interested && p.our_bitfield.HasMorePieces(p.bitfield) {
		p.am_interested = true
		p.incoming <- message{length: 1, msgId: interested}
		//log.Stderr("Peer", p.addr, "marked as interesting")
		return
	}
}

func (p *Peer) TryToRequestPiece() {
	if p.am_interested && !p.peer_choking && !p.bitfield.Completed() {
		p.requests <- PieceRequest{bitfield: p.bitfield, response: p.incoming, addr: p.addr}
	}
}

func (p *Peer) Close() {
	if p.wire != nil {
		p.pieces <- Request{msg: message{length: 1, msgId: exit, addr: p.addr}}
		p.wire.Close()
		if !closed(p.incoming) {
			close(p.incoming)
		}
	}
}

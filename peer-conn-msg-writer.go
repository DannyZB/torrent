package torrent

import (
	"bytes"
	"io"
	"time"

	"github.com/anacrolix/chansync"
	"github.com/anacrolix/log"
	"github.com/anacrolix/sync"

	pp "github.com/anacrolix/torrent/peer_protocol"
)

func (pc *PeerConn) initMessageWriter() {
	w := &pc.messageWriter
	*w = peerConnMsgWriter{
		fillWriteBuffer: func() {
			pc.locker().Lock()
			defer pc.locker().Unlock()
			if pc.closed.IsSet() {
				return
			}
			pc.fillWriteBuffer()
		},
		closed: &pc.closed,
		logger: pc.logger,
		w:      pc.w,
		keepAlive: func() bool {
			pc.locker().RLock()
			defer pc.locker().RUnlock()
			return pc.useful()
		},
		writeBuffer: new(peerConnMsgWriterBuffer),
		minFillGap:  10 * time.Millisecond, // Coalesce writes within 10ms
	}
}

func (pc *PeerConn) startMessageWriter() {
	pc.initMessageWriter()
	go pc.messageWriterRunner()
}

func (pc *PeerConn) messageWriterRunner() {
	defer pc.close()
	pc.messageWriter.run(pc.t.cl.config.KeepAliveTimeout)
}

type peerConnMsgWriterBuffer struct {
	// The number of bytes in the buffer that are part of a piece message. When
	// the whole buffer is written, we can count this many bytes.
	pieceDataBytes int
	bytes.Buffer
}

type peerConnMsgWriter struct {
	// Must not be called with the local mutex held, as it will call back into the write method.
	fillWriteBuffer func()
	closed          *chansync.SetOnce
	logger          log.Logger
	w               io.Writer
	keepAlive       func() bool

	mu        sync.Mutex
	writeCond chansync.BroadcastCond
	// Pointer so we can swap with the "front buffer".
	writeBuffer *peerConnMsgWriterBuffer

	totalWriteDuration    time.Duration
	totalBytesWritten     int64
	totalDataBytesWritten int64
	dataUploadRate        float64

	// Write coalescing to reduce lock frequency
	lastBufferFill time.Time
	minFillGap     time.Duration
}

// Routine that writes to the peer. Some of what to write is buffered by
// activity elsewhere in the Client, and some is determined locally when the
// connection is writable.
func (cn *peerConnMsgWriter) run(keepAliveTimeout time.Duration) {
	lastWrite := time.Now()
	keepAliveTimer := time.NewTimer(keepAliveTimeout)
	frontBuf := new(peerConnMsgWriterBuffer)
	for {
		if cn.closed.IsSet() {
			return
		}

		// Only call fillWriteBuffer if we have space and might need more data
		cn.mu.Lock()
		bufferHasSpace := cn.writeBuffer.Len() < writeBufferHighWaterLen
		shouldCoalesce := cn.minFillGap > 0 && time.Since(cn.lastBufferFill) < cn.minFillGap
		cn.mu.Unlock()

		if bufferHasSpace && !shouldCoalesce {
			cn.fillWriteBuffer()
			cn.mu.Lock()
			cn.lastBufferFill = time.Now()
			cn.mu.Unlock()
		}

		cn.mu.Lock()
		// Only calculate keepAlive if buffer is empty and we might need one
		var needKeepAlive bool
		bufferEmpty := cn.writeBuffer.Len() == 0
		if bufferEmpty && time.Since(lastWrite) >= keepAliveTimeout {
			needKeepAlive = cn.keepAlive()
		}

		if bufferEmpty && needKeepAlive {
			cn.writeBuffer.Write(pp.Message{Keepalive: true}.MustMarshalBinary())
			if debugMetricsEnabled {
				torrent.Add("written keepalives", 1)
			}
			bufferEmpty = false
		}
		if bufferEmpty {
			writeCond := cn.writeCond.Signaled()
			cn.mu.Unlock()
			select {
			case <-cn.closed.Done():
			case <-writeCond:
			case <-keepAliveTimer.C:
			}
			continue
		}
		// Flip the buffers.
		frontBuf, cn.writeBuffer = cn.writeBuffer, frontBuf
		cn.mu.Unlock()
		if frontBuf.Len() == 0 {
			panic("expected non-empty front buffer")
		}
		var err error
		startedWriting := time.Now()
		startingBufLen := frontBuf.Len()

		// Optimized write loop - write larger chunks when possible
		buf := frontBuf.Bytes()
		for len(buf) > 0 {
			n, writeErr := cn.w.Write(buf)
			if n > 0 {
				buf = buf[n:]
				frontBuf.Next(n)
			}
			if writeErr != nil {
				err = writeErr
				break
			}
			// Safety check: if no progress and no error, break to prevent infinite loop
			if n == 0 {
				err = io.ErrShortWrite
				break
			}
		}

		if err != nil {
			cn.logger.WithDefaultLevel(log.Debug).Printf("error writing: %v", err)
			return
		}
		// Track what was sent and how long it took.
		writeDuration := time.Since(startedWriting)
		cn.mu.Lock()
		if writeDuration.Seconds() > 0 {
			cn.dataUploadRate = float64(frontBuf.pieceDataBytes) / writeDuration.Seconds()
		}
		cn.totalWriteDuration += writeDuration
		cn.totalBytesWritten += int64(startingBufLen)
		cn.totalDataBytesWritten += int64(frontBuf.pieceDataBytes)
		cn.mu.Unlock()
		frontBuf.pieceDataBytes = 0
		lastWrite = time.Now()
		keepAliveTimer.Reset(keepAliveTimeout)
	}
}

func (cn *peerConnMsgWriter) writeToBuffer(msg pp.Message) (err error) {
	originalLen := cn.writeBuffer.Len()
	defer func() {
		if err != nil {
			// Since an error occurred during buffer write, revert buffer to its original state before the write.
			cn.writeBuffer.Truncate(originalLen)
		}
	}()
	err = msg.WriteTo(cn.writeBuffer)
	if err == nil {
		cn.writeBuffer.pieceDataBytes += len(msg.Piece)
	}
	return
}

func (cn *peerConnMsgWriter) write(msg pp.Message) bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.writeToBuffer(msg)
	cn.writeCond.Broadcast()
	return !cn.writeBufferFull()
}

func (cn *peerConnMsgWriter) writeBufferFull() bool {
	return cn.writeBuffer.Len() >= writeBufferHighWaterLen
}

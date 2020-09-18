package rtpconn

import (
	"errors"
	"io"
	"log"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"

	"sfu/conn"
	"sfu/estimator"
	"sfu/group"
	"sfu/jitter"
	"sfu/packetcache"
	"sfu/rtptime"
)

type bitrate struct {
	bitrate uint64
	jiffies uint64
}

func (br *bitrate) Set(bitrate uint64, now uint64) {
	atomic.StoreUint64(&br.bitrate, bitrate)
	atomic.StoreUint64(&br.jiffies, now)
}

func (br *bitrate) Get(now uint64) uint64 {
	ts := atomic.LoadUint64(&br.jiffies)
	if now < ts || now-ts > receiverReportTimeout {
		return ^uint64(0)
	}
	return atomic.LoadUint64(&br.bitrate)
}

type receiverStats struct {
	loss    uint32
	jitter  uint32
	jiffies uint64
}

func (s *receiverStats) Set(loss uint8, jitter uint32, now uint64) {
	atomic.StoreUint32(&s.loss, uint32(loss))
	atomic.StoreUint32(&s.jitter, jitter)
	atomic.StoreUint64(&s.jiffies, now)
}

func (s *receiverStats) Get(now uint64) (uint8, uint32) {
	ts := atomic.LoadUint64(&s.jiffies)
	if now < ts || now > ts+receiverReportTimeout {
		return 0, 0
	}
	return uint8(atomic.LoadUint32(&s.loss)), atomic.LoadUint32(&s.jitter)
}

const receiverReportTimeout = 8 * rtptime.JiffiesPerSec

type iceConnection interface {
	addICECandidate(candidate *webrtc.ICECandidateInit) error
	flushICECandidates() error
}

type rtpDownTrack struct {
	track         *webrtc.Track
	remote        conn.UpTrack
	maxBitrate    *bitrate
	rate          *estimator.Estimator
	stats         *receiverStats
	srTime        uint64
	srNTPTime     uint64
	remoteNTPTime uint64
	remoteRTPTime uint32
	cname         atomic.Value
	rtt           uint64
}

func (down *rtpDownTrack) WriteRTP(packet *rtp.Packet) error {
	return down.track.WriteRTP(packet)
}

func (down *rtpDownTrack) Accumulate(bytes uint32) {
	down.rate.Accumulate(bytes)
}

func (down *rtpDownTrack) SetTimeOffset(ntp uint64, rtp uint32) {
	atomic.StoreUint64(&down.remoteNTPTime, ntp)
	atomic.StoreUint32(&down.remoteRTPTime, rtp)
}

func (down *rtpDownTrack) SetCname(cname string) {
	down.cname.Store(cname)
}

type rtpDownConnection struct {
	id             string
	pc             *webrtc.PeerConnection
	remote         conn.Up
	tracks         []*rtpDownTrack
	maxREMBBitrate *bitrate
	iceCandidates  []*webrtc.ICECandidateInit
}

func newDownConn(c group.Client, id string, remote conn.Up) (*rtpDownConnection, error) {
	pc, err := c.Group().API().NewPeerConnection(group.IceConfiguration())
	if err != nil {
		return nil, err
	}

	pc.OnTrack(func(remote *webrtc.Track, receiver *webrtc.RTPReceiver) {
		log.Printf("Got track on downstream connection")
	})

	conn := &rtpDownConnection{
		id:             id,
		pc:             pc,
		remote:         remote,
		maxREMBBitrate: new(bitrate),
	}

	return conn, nil
}

func (down *rtpDownConnection) GetMaxBitrate(now uint64) uint64 {
	rate := down.maxREMBBitrate.Get(now)
	var trackRate uint64
	for _, t := range down.tracks {
		r := t.maxBitrate.Get(now)
		if r == ^uint64(0) {
			if t.track.Kind() == webrtc.RTPCodecTypeAudio {
				r = 128 * 1024
			} else {
				r = 512 * 1024
			}
		}
		trackRate += r
	}
	if trackRate < rate {
		return trackRate
	}
	return rate
}

func (down *rtpDownConnection) addICECandidate(candidate *webrtc.ICECandidateInit) error {
	if down.pc.RemoteDescription() != nil {
		return down.pc.AddICECandidate(*candidate)
	}
	down.iceCandidates = append(down.iceCandidates, candidate)
	return nil
}

func flushICECandidates(pc *webrtc.PeerConnection, candidates []*webrtc.ICECandidateInit) error {
	if pc.RemoteDescription() == nil {
		return errors.New("flushICECandidates called in bad state")
	}

	var err error
	for _, candidate := range candidates {
		err2 := pc.AddICECandidate(*candidate)
		if err == nil {
			err = err2
		}
	}
	return err
}

func (down *rtpDownConnection) flushICECandidates() error {
	err := flushICECandidates(down.pc, down.iceCandidates)
	down.iceCandidates = nil
	return err
}

type rtpUpTrack struct {
	track    *webrtc.Track
	label    string
	rate     *estimator.Estimator
	cache    *packetcache.Cache
	jitter   *jitter.Estimator
	lastPLI  uint64
	lastFIR  uint64
	firSeqno uint32

	localCh    chan localTrackAction
	readerDone chan struct{}

	mu        sync.Mutex
	cname     string
	local     []conn.DownTrack
	srTime    uint64
	srNTPTime uint64
	srRTPTime uint32
}

type localTrackAction struct {
	add   bool
	track conn.DownTrack
}

func (up *rtpUpTrack) notifyLocal(add bool, track conn.DownTrack) {
	select {
	case up.localCh <- localTrackAction{add, track}:
	case <-up.readerDone:
	}
}

func (up *rtpUpTrack) AddLocal(local conn.DownTrack) error {
	up.mu.Lock()
	for _, t := range up.local {
		if t == local {
			up.mu.Unlock()
			return nil
		}
	}
	up.local = append(up.local, local)
	up.mu.Unlock()

	up.notifyLocal(true, local)
	return nil
}

func (up *rtpUpTrack) DelLocal(local conn.DownTrack) bool {
	up.mu.Lock()
	for i, l := range up.local {
		if l == local {
			up.local = append(up.local[:i], up.local[i+1:]...)
			up.mu.Unlock()
			up.notifyLocal(false, l)
			return true
		}
	}
	up.mu.Unlock()
	return false
}

func (up *rtpUpTrack) getLocal() []conn.DownTrack {
	up.mu.Lock()
	defer up.mu.Unlock()
	local := make([]conn.DownTrack, len(up.local))
	copy(local, up.local)
	return local
}

func (up *rtpUpTrack) GetRTP(seqno uint16, result []byte) uint16 {
	return up.cache.Get(seqno, result)
}

func (up *rtpUpTrack) Label() string {
	return up.label
}

func (up *rtpUpTrack) Codec() *webrtc.RTPCodec {
	return up.track.Codec()
}

func (up *rtpUpTrack) hasRtcpFb(tpe, parameter string) bool {
	for _, fb := range up.track.Codec().RTCPFeedback {
		if fb.Type == tpe && fb.Parameter == parameter {
			return true
		}
	}
	return false
}

type rtpUpConnection struct {
	id            string
	label         string
	pc            *webrtc.PeerConnection
	labels        map[string]string
	iceCandidates []*webrtc.ICECandidateInit

	mu     sync.Mutex
	tracks []*rtpUpTrack
	local  []conn.Down
}

func (up *rtpUpConnection) getTracks() []*rtpUpTrack {
	up.mu.Lock()
	defer up.mu.Unlock()
	tracks := make([]*rtpUpTrack, len(up.tracks))
	copy(tracks, up.tracks)
	return tracks
}

func (up *rtpUpConnection) Id() string {
	return up.id
}

func (up *rtpUpConnection) Label() string {
	return up.label
}

func (up *rtpUpConnection) AddLocal(local conn.Down) error {
	up.mu.Lock()
	defer up.mu.Unlock()
	for _, t := range up.local {
		if t == local {
			return nil
		}
	}
	up.local = append(up.local, local)
	return nil
}

func (up *rtpUpConnection) DelLocal(local conn.Down) bool {
	up.mu.Lock()
	defer up.mu.Unlock()
	for i, l := range up.local {
		if l == local {
			up.local = append(up.local[:i], up.local[i+1:]...)
			return true
		}
	}
	return false
}

func (up *rtpUpConnection) getLocal() []conn.Down {
	up.mu.Lock()
	defer up.mu.Unlock()
	local := make([]conn.Down, len(up.local))
	copy(local, up.local)
	return local
}

func (up *rtpUpConnection) addICECandidate(candidate *webrtc.ICECandidateInit) error {
	if up.pc.RemoteDescription() != nil {
		return up.pc.AddICECandidate(*candidate)
	}
	up.iceCandidates = append(up.iceCandidates, candidate)
	return nil
}

func (up *rtpUpConnection) flushICECandidates() error {
	err := flushICECandidates(up.pc, up.iceCandidates)
	up.iceCandidates = nil
	return err
}

func getTrackMid(pc *webrtc.PeerConnection, track *webrtc.Track) string {
	for _, t := range pc.GetTransceivers() {
		if t.Receiver() != nil && t.Receiver().Track() == track {
			return t.Mid()
		}
	}
	return ""
}

// called locked
func (up *rtpUpConnection) complete() bool {
	for mid := range up.labels {
		found := false
		for _, t := range up.tracks {
			m := getTrackMid(up.pc, t.track)
			if m == mid {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func newUpConn(c group.Client, id string) (*rtpUpConnection, error) {
	pc, err := c.Group().API().NewPeerConnection(group.IceConfiguration())
	if err != nil {
		return nil, err
	}

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RtpTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)
	if err != nil {
		pc.Close()
		return nil, err
	}

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RtpTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)
	if err != nil {
		pc.Close()
		return nil, err
	}

	up := &rtpUpConnection{id: id, pc: pc}

	pc.OnTrack(func(remote *webrtc.Track, receiver *webrtc.RTPReceiver) {
		up.mu.Lock()

		mid := getTrackMid(pc, remote)
		if mid == "" {
			log.Printf("Couldn't get track's mid")
			return
		}

		label, ok := up.labels[mid]
		if !ok {
			log.Printf("Couldn't get track's label")
			isvideo := remote.Kind() == webrtc.RTPCodecTypeVideo
			if isvideo {
				label = "video"
			} else {
				label = "audio"
			}
		}

		track := &rtpUpTrack{
			track:      remote,
			label:      label,
			cache:      packetcache.New(32),
			rate:       estimator.New(time.Second),
			jitter:     jitter.New(remote.Codec().ClockRate),
			localCh:    make(chan localTrackAction, 2),
			readerDone: make(chan struct{}),
		}

		up.tracks = append(up.tracks, track)

		go readLoop(up, track)

		go rtcpUpListener(up, track, receiver)

		complete := up.complete()
		var tracks []conn.UpTrack
		if complete {
			tracks = make([]conn.UpTrack, len(up.tracks))
			for i, t := range up.tracks {
				tracks[i] = t
			}
		}

		// pushConn might need to take the lock
		up.mu.Unlock()

		if complete {
			clients := c.Group().GetClients(c)
			for _, cc := range clients {
				cc.PushConn(up.id, up, tracks, up.label)
			}
			go rtcpUpSender(up)
		}
	})

	return up, nil
}

func readLoop(conn *rtpUpConnection, track *rtpUpTrack) {
	writers := rtpWriterPool{conn: conn, track: track}
	defer func() {
		writers.close()
		close(track.readerDone)
	}()

	isvideo := track.track.Kind() == webrtc.RTPCodecTypeVideo
	buf := make([]byte, packetcache.BufSize)
	var packet rtp.Packet
	for {
		bytes, err := track.track.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("%v", err)
			}
			break
		}
		track.rate.Accumulate(uint32(bytes))

		err = packet.Unmarshal(buf[:bytes])
		if err != nil {
			log.Printf("%v", err)
			continue
		}

		track.jitter.Accumulate(packet.Timestamp)

		first, index :=
			track.cache.Store(packet.SequenceNumber, buf[:bytes])
		if packet.SequenceNumber-first > 24 {
			found, first, bitmap := track.cache.BitmapGet()
			if found {
				err := conn.sendNACK(track, first, bitmap)
				if err != nil {
					log.Printf("%v", err)
				}
			}
		}

		_, rate := track.rate.Estimate()
		delay := uint32(rtptime.JiffiesPerSec / 1024)
		if rate > 512 {
			delay = rtptime.JiffiesPerSec / rate / 2
		}

		writers.write(packet.SequenceNumber, index, delay,
			isvideo, packet.Marker)

		select {
		case action := <-track.localCh:
			err := writers.add(action.track, action.add)
			if err != nil {
				log.Printf("add/remove track: %v", err)
			}
		default:
		}
	}
}

var ErrUnsupportedFeedback = errors.New("unsupported feedback type")
var ErrRateLimited = errors.New("rate limited")

func (up *rtpUpConnection) sendPLI(track *rtpUpTrack) error {
	if !track.hasRtcpFb("nack", "pli") {
		return ErrUnsupportedFeedback
	}
	last := atomic.LoadUint64(&track.lastPLI)
	now := rtptime.Jiffies()
	if now >= last && now-last < rtptime.JiffiesPerSec/5 {
		return ErrRateLimited
	}
	atomic.StoreUint64(&track.lastPLI, now)
	return sendPLI(up.pc, track.track.SSRC())
}

func sendPLI(pc *webrtc.PeerConnection, ssrc uint32) error {
	return pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: ssrc},
	})
}

func (up *rtpUpConnection) sendFIR(track *rtpUpTrack, increment bool) error {
	// we need to reliably increment the seqno, even if we are going
	// to drop the packet due to rate limiting.
	var seqno uint8
	if increment {
		seqno = uint8(atomic.AddUint32(&track.firSeqno, 1) & 0xFF)
	} else {
		seqno = uint8(atomic.LoadUint32(&track.firSeqno) & 0xFF)
	}

	if !track.hasRtcpFb("ccm", "fir") {
		return ErrUnsupportedFeedback
	}
	last := atomic.LoadUint64(&track.lastFIR)
	now := rtptime.Jiffies()
	if now >= last && now-last < rtptime.JiffiesPerSec/5 {
		return ErrRateLimited
	}
	atomic.StoreUint64(&track.lastFIR, now)
	return sendFIR(up.pc, track.track.SSRC(), seqno)
}

func sendFIR(pc *webrtc.PeerConnection, ssrc uint32, seqno uint8) error {
	return pc.WriteRTCP([]rtcp.Packet{
		&rtcp.FullIntraRequest{
			FIR: []rtcp.FIREntry{
				{
					SSRC:           ssrc,
					SequenceNumber: seqno,
				},
			},
		},
	})
}

func (up *rtpUpConnection) sendNACK(track *rtpUpTrack, first uint16, bitmap uint16) error {
	if !track.hasRtcpFb("nack", "") {
		return nil
	}
	err := sendNACK(up.pc, track.track.SSRC(), first, bitmap)
	if err == nil {
		track.cache.Expect(1 + bits.OnesCount16(bitmap))
	}
	return err
}

func sendNACK(pc *webrtc.PeerConnection, ssrc uint32, first uint16, bitmap uint16) error {
	packet := rtcp.Packet(
		&rtcp.TransportLayerNack{
			MediaSSRC: ssrc,
			Nacks: []rtcp.NackPair{
				{
					first,
					rtcp.PacketBitmap(bitmap),
				},
			},
		},
	)
	return pc.WriteRTCP([]rtcp.Packet{packet})
}

func sendRecovery(p *rtcp.TransportLayerNack, track *rtpDownTrack) {
	var packet rtp.Packet
	buf := make([]byte, packetcache.BufSize)
	for _, nack := range p.Nacks {
		for _, seqno := range nack.PacketList() {
			l := track.remote.GetRTP(seqno, buf)
			if l == 0 {
				continue
			}
			err := packet.Unmarshal(buf[:l])
			if err != nil {
				continue
			}
			err = track.track.WriteRTP(&packet)
			if err != nil {
				log.Printf("WriteRTP: %v", err)
				continue
			}
			track.rate.Accumulate(uint32(l))
		}
	}
}

func rtcpUpListener(conn *rtpUpConnection, track *rtpUpTrack, r *webrtc.RTPReceiver) {
	for {
		firstSR := false
		ps, err := r.ReadRTCP()
		if err != nil {
			if err != io.EOF {
				log.Printf("ReadRTCP: %v", err)
			}
			return
		}

		now := rtptime.Jiffies()

		for _, p := range ps {
			local := track.getLocal()
			switch p := p.(type) {
			case *rtcp.SenderReport:
				track.mu.Lock()
				if track.srTime == 0 {
					firstSR = true
				}
				track.srTime = now
				track.srNTPTime = p.NTPTime
				track.srRTPTime = p.RTPTime
				track.mu.Unlock()
				for _, l := range local {
					l.SetTimeOffset(p.NTPTime, p.RTPTime)
				}
			case *rtcp.SourceDescription:
				for _, c := range p.Chunks {
					if c.Source != track.track.SSRC() {
						continue
					}
					for _, i := range c.Items {
						if i.Type != rtcp.SDESCNAME {
							continue
						}
						track.mu.Lock()
						track.cname = i.Text
						track.mu.Unlock()
						for _, l := range local {
							l.SetCname(i.Text)
						}
					}
				}
			}
		}

		if firstSR {
			// this is the first SR we got for at least one track,
			// quickly propagate the time offsets downstream
			local := conn.getLocal()
			for _, l := range local {
				l, ok := l.(*rtpDownConnection)
				if ok {
					err := sendSR(l)
					if err != nil {
						log.Printf("sendSR: %v", err)
					}
				}
			}
		}
	}
}

func sendUpRTCP(conn *rtpUpConnection) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if len(conn.tracks) == 0 {
		state := conn.pc.ConnectionState()
		if state == webrtc.PeerConnectionStateClosed {
			return io.ErrClosedPipe
		}
		return nil
	}

	now := rtptime.Jiffies()

	reports := make([]rtcp.ReceptionReport, 0, len(conn.tracks))
	for _, t := range conn.tracks {
		updateUpTrack(t)
		expected, lost, totalLost, eseqno := t.cache.GetStats(true)
		if expected == 0 {
			expected = 1
		}
		if lost >= expected {
			lost = expected - 1
		}

		t.mu.Lock()
		srTime := t.srTime
		srNTPTime := t.srNTPTime
		t.mu.Unlock()

		var delay uint64
		if srTime != 0 {
			delay = (now - srTime) /
				(rtptime.JiffiesPerSec / 0x10000)
		}

		reports = append(reports, rtcp.ReceptionReport{
			SSRC:               t.track.SSRC(),
			FractionLost:       uint8((lost * 256) / expected),
			TotalLost:          totalLost,
			LastSequenceNumber: eseqno,
			Jitter:             t.jitter.Jitter(),
			LastSenderReport:   uint32(srNTPTime >> 16),
			Delay:              uint32(delay),
		})
	}

	packets := []rtcp.Packet{
		&rtcp.ReceiverReport{
			Reports: reports,
		},
	}

	rate := ^uint64(0)
	for _, l := range conn.local {
		r := l.GetMaxBitrate(now)
		if r < rate {
			rate = r
		}
	}
	if rate < group.MinBitrate {
		rate = group.MinBitrate
	}

	var ssrcs []uint32
	for _, t := range conn.tracks {
		if t.hasRtcpFb("goog-remb", "") {
			continue
		}
		ssrcs = append(ssrcs, t.track.SSRC())
	}

	if len(ssrcs) > 0 {
		packets = append(packets,
			&rtcp.ReceiverEstimatedMaximumBitrate{
				Bitrate: rate,
				SSRCs:   ssrcs,
			},
		)
	}
	return conn.pc.WriteRTCP(packets)
}

func rtcpUpSender(conn *rtpUpConnection) {
	for {
		time.Sleep(time.Second)
		err := sendUpRTCP(conn)
		if err != nil {
			if err == io.EOF || err == io.ErrClosedPipe {
				return
			}
			log.Printf("sendRR: %v", err)
		}
	}
}

func sendSR(conn *rtpDownConnection) error {
	// since this is only called after all tracks have been created,
	// there is no need for locking.
	packets := make([]rtcp.Packet, 0, len(conn.tracks))

	now := time.Now()
	nowNTP := rtptime.TimeToNTP(now)
	jiffies := rtptime.TimeToJiffies(now)

	for _, t := range conn.tracks {
		clockrate := t.track.Codec().ClockRate

		var nowRTP uint32

		remoteNTP := atomic.LoadUint64(&t.remoteNTPTime)
		remoteRTP := atomic.LoadUint32(&t.remoteRTPTime)
		if remoteNTP != 0 {
			srTime := rtptime.NTPToTime(remoteNTP)
			d := now.Sub(srTime)
			if d > 0 && d < time.Hour {
				delay := rtptime.FromDuration(
					d, clockrate,
				)
				nowRTP = remoteRTP + uint32(delay)
			}

			p, b := t.rate.Totals()
			packets = append(packets,
				&rtcp.SenderReport{
					SSRC:        t.track.SSRC(),
					NTPTime:     nowNTP,
					RTPTime:     nowRTP,
					PacketCount: p,
					OctetCount:  b,
				})
			atomic.StoreUint64(&t.srTime, jiffies)
			atomic.StoreUint64(&t.srNTPTime, nowNTP)
		}

		cname, ok := t.cname.Load().(string)
		if ok {
			item := rtcp.SourceDescriptionItem{
				Type: rtcp.SDESCNAME,
				Text: cname,
			}
			packets = append(packets,
				&rtcp.SourceDescription{
					Chunks: []rtcp.SourceDescriptionChunk{
						{
							Source: t.track.SSRC(),
							Items:  []rtcp.SourceDescriptionItem{item},
						},
					},
				},
			)
		}
	}

	if len(packets) == 0 {
		state := conn.pc.ConnectionState()
		if state == webrtc.PeerConnectionStateClosed {
			return io.ErrClosedPipe
		}
		return nil
	}

	return conn.pc.WriteRTCP(packets)
}

func rtcpDownSender(conn *rtpDownConnection) {
	for {
		time.Sleep(time.Second)
		err := sendSR(conn)
		if err != nil {
			if err == io.EOF || err == io.ErrClosedPipe {
				return
			}
			log.Printf("sendSR: %v", err)
		}
	}
}

const (
	minLossRate  = 9600
	initLossRate = 512 * 1000
	maxLossRate  = 1 << 30
)

func (track *rtpDownTrack) updateRate(loss uint8, now uint64) {
	rate := track.maxBitrate.Get(now)
	if rate < minLossRate || rate > maxLossRate {
		// no recent feedback, reset
		rate = initLossRate
	}
	if loss < 5 {
		// if our actual rate is low, then we're not probing the
		// bottleneck
		r, _ := track.rate.Estimate()
		actual := 8 * uint64(r)
		if actual >= (rate*7)/8 {
			// loss < 0.02, multiply by 1.05
			rate = rate * 269 / 256
			if rate > maxLossRate {
				rate = maxLossRate
			}
		}
	} else if loss > 25 {
		// loss > 0.1, multiply by (1 - loss/2)
		rate = rate * (512 - uint64(loss)) / 512
		if rate < minLossRate {
			rate = minLossRate
		}
	}

	// update unconditionally, to set the timestamp
	track.maxBitrate.Set(rate, now)
}

func rtcpDownListener(conn *rtpDownConnection, track *rtpDownTrack, s *webrtc.RTPSender) {
	var gotFir bool
	lastFirSeqno := uint8(0)

	for {
		ps, err := s.ReadRTCP()
		if err != nil {
			if err != io.EOF {
				log.Printf("ReadRTCP: %v", err)
			}
			return
		}
		jiffies := rtptime.Jiffies()

		for _, p := range ps {
			switch p := p.(type) {
			case *rtcp.PictureLossIndication:
				remote, ok := conn.remote.(*rtpUpConnection)
				if !ok {
					continue
				}
				rt, ok := track.remote.(*rtpUpTrack)
				if !ok {
					continue
				}
				err := remote.sendPLI(rt)
				if err != nil && err != ErrRateLimited {
					log.Printf("sendPLI: %v", err)
				}
			case *rtcp.FullIntraRequest:
				found := false
				var seqno uint8
				for _, entry := range p.FIR {
					if entry.SSRC == track.track.SSRC() {
						found = true
						seqno = entry.SequenceNumber
						break
					}
				}
				if !found {
					log.Printf("Misdirected FIR")
					continue
				}

				increment := true
				if gotFir {
					increment = seqno != lastFirSeqno
				}
				gotFir = true
				lastFirSeqno = seqno

				remote, ok := conn.remote.(*rtpUpConnection)
				if !ok {
					continue
				}
				rt, ok := track.remote.(*rtpUpTrack)
				if !ok {
					continue
				}
				err := remote.sendFIR(rt, increment)
				if err == ErrUnsupportedFeedback {
					err := remote.sendPLI(rt)
					if err != nil && err != ErrRateLimited {
						log.Printf("sendPLI: %v", err)
					}
				} else if err != nil {
					log.Printf("sendFIR: %v", err)
				}
			case *rtcp.ReceiverEstimatedMaximumBitrate:
				conn.maxREMBBitrate.Set(p.Bitrate, jiffies)
			case *rtcp.ReceiverReport:
				for _, r := range p.Reports {
					if r.SSRC == track.track.SSRC() {
						handleReport(track, r, jiffies)
					}
				}
			case *rtcp.SenderReport:
				for _, r := range p.Reports {
					if r.SSRC == track.track.SSRC() {
						handleReport(track, r, jiffies)
					}
				}
			case *rtcp.TransportLayerNack:
				sendRecovery(p, track)
			}
		}
	}
}

func handleReport(track *rtpDownTrack, report rtcp.ReceptionReport, jiffies uint64) {
	track.stats.Set(report.FractionLost, report.Jitter, jiffies)
	track.updateRate(report.FractionLost, jiffies)

	if report.LastSenderReport != 0 {
		jiffies := rtptime.Jiffies()
		srTime := atomic.LoadUint64(&track.srTime)
		if jiffies < srTime || jiffies-srTime > 8*rtptime.JiffiesPerSec {
			return
		}
		srNTPTime := atomic.LoadUint64(&track.srNTPTime)
		if report.LastSenderReport == uint32(srNTPTime>>16) {
			delay := uint64(report.Delay) *
				(rtptime.JiffiesPerSec / 0x10000)
			if delay > jiffies-srTime {
				return
			}
			rtt := (jiffies - srTime) - delay
			oldrtt := atomic.LoadUint64(&track.rtt)
			newrtt := rtt
			if oldrtt > 0 {
				newrtt = (3*oldrtt + rtt) / 4
			}
			atomic.StoreUint64(&track.rtt, newrtt)
		}
	}
}

func updateUpTrack(track *rtpUpTrack) {
	now := rtptime.Jiffies()

	clockrate := track.track.Codec().ClockRate
	local := track.getLocal()
	var maxrto uint64
	for _, l := range local {
		ll, ok := l.(*rtpDownTrack)
		if ok {
			_, j := ll.stats.Get(now)
			jitter := uint64(j) *
				(rtptime.JiffiesPerSec / uint64(clockrate))
			rtt := atomic.LoadUint64(&ll.rtt)
			rto := rtt + 4*jitter
			if rto > maxrto {
				maxrto = rto
			}
		}
	}
	_, r := track.rate.Estimate()
	packets := int((uint64(r) * maxrto * 4) / rtptime.JiffiesPerSec)
	if packets < 32 {
		packets = 32
	}
	if packets > 256 {
		packets = 256
	}
	track.cache.ResizeCond(packets)
}
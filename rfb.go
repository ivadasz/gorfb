package gorfb

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"log"
	"net"
)

type (
	RfbServer struct {
		ln    net.Listener
		Input chan InputEvent
		Txt   chan CutEvent
		Getfb chan draw.Image
		Relfb chan []image.Rectangle
	}
	dirtyRect struct {
		image.Rectangle
		req int
	}
	getUpdate struct {
		Dirty
		outch chan [][]byte
	}
	rfbMuxState struct {
		conlist []*net.Conn
		input   chan InputEvent
		cut     chan CutEvent
	}
	muxMsg interface {
		work(state *rfbMuxState)
	}

	InputEvent struct {
		T    int
		Key  uint32
		Pos  image.Point
		Mask uint8
	}
	CutEvent struct {
		Txt string
	}
	muxNewConn struct {
		conn *net.Conn
	}
	muxDelConn struct {
		conn *net.Conn
	}
)

const (
	serverVersion = "RFB 003.008\n"
)

const (
	RFB_SET_PIXEL_FORMAT           = 0
	RFB_SET_ENCODINGS              = 2
	RFB_FRAMEBUFFER_UPDATE_REQUEST = 3
	RFB_KEY_EVENT                  = 4
	RFB_POINTER_EVENT              = 5
	RFB_CLIENT_CUT_TEXT            = 6
)

const (
	RFB_FRAMEBUFFER_UPDATE    = 0
	RFB_SET_COLOR_MAP_ENTRIES = 1
	RFB_BELL                  = 2
	RFB_SERVER_CUT_TEXT       = 3
)

const (
	RFB_ENCODING_RAW      = 0
	RFB_ENCODING_COPYRECT = 1
	// XXX
)

func makePixelFormat() []byte {
	b := make([]byte, 16)

	b[0] = 32
	b[1] = 24
	b[2] = 0
	b[3] = 1
	binary.BigEndian.PutUint16(b[4:6], 255)
	binary.BigEndian.PutUint16(b[6:8], 255)
	binary.BigEndian.PutUint16(b[8:10], 255)
	b[10] = 16
	b[11] = 8
	b[12] = 0
	b[13] = 0 // padding
	b[14] = 0 // padding
	b[15] = 0 // padding

	return b
}

func makeServerInit(name string, bounds image.Rectangle) []byte {
	b := make([]byte, 24+len(name))

	// framebuffer-width in pixels
	binary.BigEndian.PutUint16(b[0:2], uint16(bounds.Dx()))

	// framebuff-height in pixels
	binary.BigEndian.PutUint16(b[2:4], uint16(bounds.Dy()))

	// server-pixel-format
	copy(b[4:20], makePixelFormat())

	// name-length
	binary.BigEndian.PutUint32(b[20:24], uint32(len(name)))

	// name-string
	copy(b[24:], []byte(name))

	return b
}

func reasonmsg(conn net.Conn, s string) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(len(s)))
	conn.Write(b)
	fmt.Fprint(conn, s)
}

func getClientRfbVersion(conn net.Conn) (string, error) {
	b := make([]byte, 12)
	n, err := conn.Read(b)
	if err != nil || n != 12 {
		return "", err
	}
	return string(b), nil
}

func makeServerSecurities() []byte {
	return []byte{1, 1}
}

func getClientSecurity(conn net.Conn) (uint8, error) {
	c := make([]byte, 1)
	n, err := conn.Read(c)
	if err != nil || n != 1 {
		return 0, err
	}
	return uint8(c[0]), nil
}

func makeHandshake(x uint8) []byte {
	return []byte{0, 0, 0, byte(x)}
}

func getSharedFlag(conn net.Conn) (bool, error) {
	d := make([]byte, 1)
	n, err := conn.Read(d)
	if err != nil || n != 1 {
		return true, err
	}
	return d[0] == 1, nil
}

func dirtyTracker(ch <-chan dirtyRect, fbch chan getUpdate, outch chan [][]byte, regch <-chan chan []image.Rectangle, unregch chan chan []image.Rectangle) {
	var msg dirtyRect

	wanted := image.Rect(0, 0, 0, 0)
	dirty := mkclean()

	reg := <-regch
	defer func() {
		unregch <- reg
	}()

	for {
		if dirty.intersect(wanted).empty() {
			select {
			case msg := <-ch:
				{
					if msg.req == 1 {
						dirty = dirty.add(msg.Rectangle)
					} else {
						wanted = msg.Rectangle
					}
				}
			case a := <-reg:
				{
					for _, b := range a {
						dirty = dirty.add(b)
					}
				}
			}
		} else {
			select {
			case msg = <-ch:
				{
					if msg.req == 1 {
						dirty = dirty.add(msg.Rectangle)
					} else {
						wanted = msg.Rectangle
					}
				}
			case a := <-reg:
				{
					for _, b := range a {
						dirty = dirty.add(b)
					}
				}

			// This happends only when we can immediately read
			// the image data as well.
			case fbch <- getUpdate{dirty.intersect(wanted), outch}:
				{
					// reset the wanted and dirty image.Rectangle
					wanted = image.Rect(0, 0, 0, 0)
					dirty = mkclean()
				}
			}
		}
	}
}

func clientInput(in io.Reader, ctl chan interface{}, mux chan muxMsg, dt chan dirtyRect) {
	defer func() {
		ctl <- nil
	}()

	b := make([]byte, 1)
	for {
		n, err := in.Read(b)
		if err != nil || n != 1 {
			log.Print(err)
			return
		}
		switch b[0] {
		case RFB_SET_PIXEL_FORMAT:
			{
				b := make([]byte, 19)
				n, err := in.Read(b)
				if err != nil || n != 19 {
					log.Print(err)
					return
				}
				fmt.Printf("Pixel Format: %v\n", b[3:19])
			}
		case RFB_SET_ENCODINGS:
			{
				b := make([]byte, 3)
				n, err := in.Read(b)
				if err != nil || n != 3 {
					log.Print(err)
					return
				}
				m := binary.BigEndian.Uint16(b[1:3])
				for i := 0; i < int(m); i++ {
					c := make([]byte, 4)
					n, err := in.Read(c)
					if err != nil || n != 4 {
						log.Print(err)
						return
					}
					fmt.Printf("Encoding type: %d\n", int32(binary.BigEndian.Uint32(c)))
				}
			}
		case RFB_FRAMEBUFFER_UPDATE_REQUEST:
			{
				b := make([]byte, 9)
				n, err := in.Read(b)
				if err != nil || n != 9 {
					log.Print(err)
					return
				}
				incr := b[0] == 1
				x := int(binary.BigEndian.Uint16(b[1:3]))
				y := int(binary.BigEndian.Uint16(b[3:5]))
				w := int(binary.BigEndian.Uint16(b[5:7]))
				h := int(binary.BigEndian.Uint16(b[7:9]))
				//				fmt.Printf("UpdateRequest: incr=%v x=%v y=%v w=%v h=%v\n", incr, x, y, w, h)
				// Send the viewport of our remote client to
				// the dirtyTracker goroutine.
				dt <- dirtyRect{image.Rect(x, y, x+w, y+h), 0}

				// Signal our viewport as dirty if
				// non-incremental update is signaled by our
				// client.
				if !incr {
					dt <- dirtyRect{image.Rect(x, y, x+w, y+h), 1}
				}
			}
		case RFB_KEY_EVENT:
			{
				var b [7]byte
				n, err := in.Read(b[:])
				if err != nil || n != 7 {
					log.Print(err)
					return
				}
				mux <- kbdEvent(b)
			}
		case RFB_POINTER_EVENT:
			{
				var b [5]byte
				n, err := in.Read(b[:])
				if err != nil || n != 5 {
					log.Print(err)
					return
				}
				mux <- ptrEvent(b)
			}
		case RFB_CLIENT_CUT_TEXT:
			{
				b := make([]byte, 7)
				n, err := in.Read(b)
				if err != nil || n != 7 {
					log.Print(err)
					return
				}
				length := binary.BigEndian.Uint32(b[3:7])
				c := make([]byte, length)
				n, err = in.Read(c)
				if err != nil || n != int(length) {
					log.Print(err)
					return
				}
				mux <- cutEvent(c)
			}
		}
	}
}

func clientOutput(out io.Writer, ctl chan interface{}, ch <-chan [][]byte) {
	defer func() {
		ctl <- nil
	}()

	for b := range ch {
		for _, c := range b {
			out.Write(c)
		}
	}
}

func initializeConnection(conn net.Conn, bounds image.Rectangle) {
	fmt.Fprint(conn, serverVersion)
	clientVersion, err := getClientRfbVersion(conn)
	if err != nil {
		return
	}
	if clientVersion != serverVersion {
		conn.Write([]byte{0})
		reasonmsg(conn, "Unsupported")
		return
	}

	conn.Write(makeServerSecurities())
	cs, err := getClientSecurity(conn)
	if err != nil {
		return
	}
	fmt.Printf("chosen security: %v\n", cs)

	if cs != 1 {
		conn.Write(makeHandshake(1))
		reasonmsg(conn, fmt.Sprintf("Unsupported security type %v", cs))
		return
	}
	conn.Write(makeHandshake(0))

	// Initialization
	shared, err := getSharedFlag(conn)
	if err != nil {
		return
	}
	fmt.Printf("shared: %v\n", shared)
	conn.Write(makeServerInit("GoRFB", bounds))
}

func handleConn(conn net.Conn, bounds image.Rectangle, mux chan muxMsg, fbch chan getUpdate, regch <-chan chan []image.Rectangle, unregch chan chan []image.Rectangle) {
	defer conn.Close()
	initializeConnection(conn, bounds)

	mux <- muxNewConn{&conn}
	defer func() {
		mux <- muxDelConn{&conn}
	}()
	ctl := make(chan interface{})
	dt := make(chan dirtyRect)
	outch := make(chan [][]byte)

	go dirtyTracker(dt, fbch, outch, regch, unregch)
	go clientInput(conn, ctl, mux, dt)
	go clientOutput(conn, ctl, outch)

	<-ctl // clientInput or -Output exits
	<-ctl // clientInput or -Output exits
	close(ctl)
	close(outch)
	close(dt) // stop dirtyTracker
}

func accepter(ln net.Listener, bounds image.Rectangle, mux chan muxMsg, fbch chan getUpdate, regch <-chan chan []image.Rectangle, unregch chan chan []image.Rectangle) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Print(err)
			return
		}
		go handleConn(conn, bounds, mux, fbch, regch, unregch)
	}
}

func ptrEvent(b [5]byte) InputEvent {
	mask := uint8(b[0])
	x := int(binary.BigEndian.Uint16(b[1:3]))
	y := int(binary.BigEndian.Uint16(b[3:5]))
	return InputEvent{T: 0, Pos: image.Point{x, y}, Mask: mask}
}

func kbdEvent(b [7]byte) InputEvent {
	downflag := uint8(b[0])
	key := binary.BigEndian.Uint32(b[3:7])
	return InputEvent{T: 1, Key: key, Mask: downflag}
}

func cutEvent(b []byte) CutEvent {
	return CutEvent{string(b)}
}

func (ev InputEvent) encode() []byte {
	var b []byte
	if ev.T == 0 {
		// Mouse Event
		b = make([]byte, 6)
		b[0] = byte(uint8(RFB_POINTER_EVENT))
		b[1] = byte(ev.Mask)
		binary.BigEndian.PutUint16(b[2:4], uint16(ev.Pos.X))
		binary.BigEndian.PutUint16(b[4:6], uint16(ev.Pos.Y))
	} else {
		// Keyboard Event
		b = make([]byte, 8)
		b[0] = byte(uint8(RFB_KEY_EVENT))
		b[1] = byte(ev.Mask)
		binary.BigEndian.PutUint32(b[4:8], ev.Key)
	}
	return b
}

func (ev CutEvent) encode() []byte {
	b := make([]byte, 8+len(ev.Txt))

	b[0] = byte(RFB_CLIENT_CUT_TEXT)
	binary.BigEndian.PutUint32(b[4:8], uint32(len(ev.Txt)))
	copy(b[8:], ev.Txt)

	return b
}

func (ev InputEvent) work(state *rfbMuxState) {
	state.input <- ev
}

func (ev CutEvent) work(state *rfbMuxState) {
	state.cut <- ev
}

func (ev muxNewConn) work(state *rfbMuxState) {
	state.conlist = append(state.conlist, ev.conn)
}

func (ev muxDelConn) work(state *rfbMuxState) {
	for i, conn := range state.conlist {
		if conn == ev.conn {
			if i+1 < len(state.conlist) {
				state.conlist = append(state.conlist[:i], state.conlist[i+1:]...)
			} else {
				state.conlist = state.conlist[:i]
			}
		}
	}
}

func rfbMux(ch <-chan muxMsg, serv *RfbServer) {
	state := rfbMuxState{[]*net.Conn{}, serv.Input, serv.Txt}

	for msg := range ch {
		msg.work(&state)
	}
}

func encodeRect(img image.Image, rect image.Rectangle, b [][]byte) {
	// Rectangle header
	nextbuf := make([]byte, 12)
	x := rect.Min.X
	y := rect.Min.Y
	w := rect.Dx()
	h := rect.Dy()
	binary.BigEndian.PutUint16(nextbuf[0:2], uint16(x))
	binary.BigEndian.PutUint16(nextbuf[2:4], uint16(y))
	binary.BigEndian.PutUint16(nextbuf[4:6], uint16(w))
	binary.BigEndian.PutUint16(nextbuf[6:8], uint16(h))
	encoding := uint32(int32(RFB_ENCODING_RAW))
	binary.BigEndian.PutUint32(nextbuf[8:12], encoding)

	// Rectangle data
	rawbuf := make([]byte, w*h*4)
	for i := 0; i < h; i++ {
		for j := 0; j < w; j++ {
			r, g, b, a := img.At(x+j, y+i).RGBA()
			rawbuf[i*int(w)*4+j*4] = byte(uint8(b))
			rawbuf[i*int(w)*4+j*4+1] = byte(uint8(g))
			rawbuf[i*int(w)*4+j*4+2] = byte(uint8(r))
			rawbuf[i*int(w)*4+j*4+3] = byte(uint8(a))
		}
	}

	b[0] = nextbuf
	b[1] = rawbuf
}

func encodeDirty(img image.Image, dirt Dirty) [][]byte {
	rs := dirt.toRects()
	nrects := len(rs)

	if nrects == 0 {
		return [][]byte{}
	}

	outbytes := make([][]byte, 2*nrects+1)
	outbuf := make([]byte, 4)
	outbuf[0] = RFB_FRAMEBUFFER_UPDATE
	outbuf[1] = 0 // padding
	binary.BigEndian.PutUint16(outbuf[2:4], uint16(nrects))
	outbytes[0] = outbuf
	for i, r := range rs {
		encodeRect(img, r, outbytes[2*i+1:2*i+3])
	}
	return outbytes
}

func updater(img draw.Image, fbch <-chan getUpdate, serv *RfbServer, regch chan chan []image.Rectangle, unregch <-chan chan []image.Rectangle) {
	reglist := []chan []image.Rectangle{}
	defer func() {
		for _, ch := range reglist {
			close(ch)
		}
	}()
	ch := make(chan []image.Rectangle)
	defer close(ch)

	for {
		select {
		case serv.Getfb <- img:
			{
				d := <-serv.Relfb
				// signal the d image.Rectangle to all the dirtyTrackers
				for _, reg := range reglist {
					reg <- d
				}
			}
		case a := <-fbch:
			{
				a.outch <- encodeDirty(img, a.Dirty)
			}
		case regch <- ch:
			{
				reglist = append(reglist, ch)
				ch = make(chan []image.Rectangle)
			}
		case a := <-unregch:
			{
				for i, c := range reglist {
					if a == c {
						if i+1 < len(reglist) {
							reglist = append(reglist[:i], reglist[i+1:]...)
						} else {
							reglist = reglist[:i]
						}
						close(a)
					}
				}
			}
		}
	}
}

func serve(port string, img draw.Image, serv *RfbServer) {
	muxch := make(chan muxMsg)
	defer close(muxch)
	fbch := make(chan getUpdate)
	defer close(fbch)
	regch := make(chan chan []image.Rectangle)
	defer close(regch)
	unregch := make(chan chan []image.Rectangle)
	defer close(unregch)

	go rfbMux(muxch, serv)
	go updater(img, fbch, serv, regch, unregch)

	// go accepter(ln, muxch, fbch)
	accepter(serv.ln, img.Bounds(), muxch, fbch, regch, unregch)
}

func Server(port string, img draw.Image) (*RfbServer, error) {
	ln, err := net.Listen("tcp", port)
	if err != nil {
		return nil, err
	}
	input := make(chan InputEvent)
	txt := make(chan CutEvent)
	getfb := make(chan draw.Image)
	relfb := make(chan []image.Rectangle)

	serv := &RfbServer{ln, input, txt, getfb, relfb}
	go serve(":5900", img, serv)

	return serv, nil
}

func ServeDumbFb(w uint16, h uint16) (*RfbServer, error) {
	img := image.NewRGBA(image.Rect(0, 0, int(w), int(h)))
	black := color.RGBA{0, 0, 0, 0}
	draw.Draw(img, image.Rect(0, 0, int(w), int(h)), &image.Uniform{black}, image.ZP, draw.Src)

	serv, err := Server(":5900", img)
	if err != nil {
		return nil, err
	}

	return serv, nil
}

func (serv *RfbServer) Shutdown() {
	serv.ln.Close()
	close(serv.Input)
	close(serv.Txt)
	close(serv.Getfb)
	close(serv.Relfb)
}

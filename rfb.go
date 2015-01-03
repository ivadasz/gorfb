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
		ln      net.Listener
		Input   chan InputEvent
		Txt     chan CutEvent
		Getfb   chan draw.Image
		Relfb   chan []image.Rectangle
		regch   chan chan []image.Rectangle
		unregch chan chan []image.Rectangle
	}
	RfbClient struct {
		conn    net.Conn
		bounds  image.Rectangle
		mux     chan muxMsg
		regch   <-chan chan []image.Rectangle
		unregch chan chan []image.Rectangle
	}
	PixelFormat struct {
		bpp, depth, beflag, trueColor   uint8
		redMax, greenMax, blueMax       uint16
		redShift, greenShift, blueShift uint8
	}
	encodings    []int32
	serverStatus struct {
		fbWidth, fbHeight int
		format            PixelFormat
		name              string
	}
	updateRect struct {
		image.Rectangle
		incr bool
	}
	encodable interface {
		encode() []byte
	}
	getUpdate struct {
		Dirty
		outch chan [][]byte
	}
	rfbMuxState struct {
		input chan InputEvent
		cut   chan CutEvent
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
)

const (
	serverVersion = "RFB 003.008\n"
)

const (
	setPixelFormatReq    = 0
	setEncodingsReq      = 2
	framebufferUpdateReq = 3
	keyEventReq          = 4
	pointerEventReq      = 5
	clientCutTextReq     = 6
)

const (
	framebufferUpdateMsg  = 0
	setColorMapEntriesMsg = 1
	bellMsg               = 2
	serverCutTextMsg      = 3
)

const (
	encodingRaw      = 0
	encodingCopyrect = 1
	// XXX
)

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

func dirtyTracker(ch <-chan updateRect, fbch chan getUpdate, outch chan [][]byte, reg <-chan []image.Rectangle, done <-chan interface{}) {
	var msg updateRect

	wanted := image.Rect(0, 0, 0, 0)
	dirty := mkclean()
	nextdata := [][]byte{}
	updata := make(chan [][]byte)
	update_pending := false
	defer func() {
		if update_pending {
			<-updata
			update_pending = false
		}
		close(updata)
	}()

	for {
		if dirty.intersect(wanted).empty() && len(nextdata) == 0 {
			select {
			case <-done:
				{
					return
				}
			case d := <-updata:
				{
					update_pending = false
					nextdata = append(nextdata, d...)
				}
			case msg := <-ch:
				{
					wanted = msg.Rectangle
					if !msg.incr {
						dirty = dirty.add(msg.Rectangle)
					}
				}
			case a := <-reg:
				{
					for _, b := range a {
						dirty = dirty.add(b)
					}
				}
			}
		} else if dirty.intersect(wanted).empty() {
			select {
			case <-done:
				{
					return
				}
			case d := <-updata:
				{
					update_pending = false
					nextdata = append(nextdata, d...)
				}
			case outch <- nextdata:
				{
					nextdata = [][]byte{}
				}
			case msg := <-ch:
				{
					wanted = msg.Rectangle
					if !msg.incr {
						dirty = dirty.add(msg.Rectangle)
					}
				}
			case a := <-reg:
				{
					for _, b := range a {
						dirty = dirty.add(b)
					}
				}
			}
		} else if len(nextdata) == 0 {
			select {
			case <-done:
				{
					return
				}
			case d := <-updata:
				{
					update_pending = false
					nextdata = append(nextdata, d...)
				}
			case msg = <-ch:
				{
					wanted = msg.Rectangle
					if !msg.incr {
						dirty = dirty.add(msg.Rectangle)
					}
				}
			case a := <-reg:
				{
					for _, b := range a {
						dirty = dirty.add(b)
					}
				}
			// This happens only when we can immediately read
			// the image data as well.
			case fbch <- getUpdate{dirty.intersect(wanted), updata}:
				{
					// reset the wanted and dirty image.Rectangle
					wanted = image.Rect(0, 0, 0, 0)
					dirty = mkclean()
				}
			}
		} else {
			select {
			case <-done:
				{
					return
				}
			case outch <- nextdata:
				{
					update_pending = false
					nextdata = [][]byte{}
				}
			case d := <-updata:
				{
					nextdata = append(nextdata, d...)
				}
			case msg = <-ch:
				{
					wanted = msg.Rectangle
					if !msg.incr {
						dirty = dirty.add(msg.Rectangle)
					}
				}
			case a := <-reg:
				{
					for _, b := range a {
						dirty = dirty.add(b)
					}
				}
			// This happens only when we can immediately read
			// the image data as well.
			case fbch <- getUpdate{dirty.intersect(wanted), updata}:
				{
					update_pending = true
					// reset the wanted and dirty image.Rectangle
					wanted = image.Rect(0, 0, 0, 0)
					dirty = mkclean()
				}
			}
		}
	}
}

func clientInput(in io.Reader, mux chan muxMsg, dt chan updateRect, done <-chan interface{}) {
	b := make([]byte, 1)
	for {
		n, err := in.Read(b)
		if err != nil || n != 1 {
			log.Print(err)
			return
		}
		switch b[0] {
		case setPixelFormatReq:
			{
				var b [19]byte
				var c [16]byte
				n, err := in.Read(b[:])
				if err != nil || n != 19 {
					log.Print(err)
					return
				}
				copy(c[:], b[3:])
				format := decodePixelFormat(c)
				fmt.Printf("Pixel Format: %v\n", format)
			}
		case setEncodingsReq:
			{
				var b [3]byte
				n, err := in.Read(b[:])
				if err != nil || n != 3 {
					log.Print(err)
					return
				}
				m := binary.BigEndian.Uint16(b[1:3])
				c := make([]byte, 4*m)
				for i := 0; i < int(m); i++ {
					n, err := in.Read(c[4*i : 4*(i+1)])
					if err != nil || n != 4 {
						log.Print(err)
						return
					}
				}
				ls := decodeEncodings(c)
				fmt.Printf("Encodings: %v\n", ls)
			}
		case framebufferUpdateReq:
			{
				var b [9]byte
				n, err := in.Read(b[:])
				if err != nil || n != 9 {
					log.Print(err)
					return
				}
				select {
				case <-done:
					{
						return
					}
				case dt <- updateRequest(b):
				}
			}
		case keyEventReq:
			{
				var b [7]byte
				n, err := in.Read(b[:])
				if err != nil || n != 7 {
					log.Print(err)
					return
				}
				select {
				case <-done:
					{
						return
					}
				case mux <- kbdEvent(b):
				}
			}
		case pointerEventReq:
			{
				var b [5]byte
				n, err := in.Read(b[:])
				if err != nil || n != 5 {
					log.Print(err)
					return
				}
				select {
				case <-done:
					{
						return
					}
				case mux <- ptrEvent(b):
				}
			}
		case clientCutTextReq:
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
				select {
				case <-done:
					{
						return
					}
				case mux <- cutEvent(c):
				}
			}
		}
	}
}

func clientOutput(out io.Writer, ch <-chan [][]byte, done <-chan interface{}) {
	for {
		select {
		case <-done:
			{
				return
			}
		case b := <-ch:
			{
				for _, c := range b {
					n, err := out.Write(c)
					if err != nil || n < len(c) {
						log.Print(err)
						return
					}
				}
			}
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
	format := PixelFormat{32, 24, 0, 1, 255, 255, 255, 16, 8, 0}
	serverInit := serverStatus{bounds.Dx(), bounds.Dy(), format, "GoRFB"}
	conn.Write(serverInit.encode())
}

func handleConn(client *RfbClient, fbch chan getUpdate) {
	initializeConnection(client.conn, client.bounds)

	dt := make(chan updateRect)
	outch := make(chan [][]byte)

	snc := NewSyncer()

	snc.Add("net.Conn closer")
	go func() {
		defer snc.Killed("net.Conn closer")
		defer client.conn.Close()
		<-snc.Killer
	}()

	snc.Add("dirtyTracker")
	go func() {
		reg := <-client.regch
		defer snc.Killed("dirtyTracker")
		defer func() {
			client.unregch <- reg
		}()
		dirtyTracker(dt, fbch, outch, reg, snc.Killer)
	}()
	snc.Add("clientInput")
	go func() {
		defer snc.Killed("clientInput")
		clientInput(client.conn, client.mux, dt, snc.Killer)
	}()
	snc.Add("clientInput")
	go func() {
		defer snc.Killed("clientOutput")
		clientOutput(client.conn, outch, snc.Killer)
	}()

	snc.Wait()
	close(outch)
	close(dt)
}

func accepter(serv *RfbServer, bounds image.Rectangle, mux chan muxMsg, fbch chan getUpdate) {
	for {
		conn, err := serv.ln.Accept()
		if err != nil {
			log.Print(err)
			return
		}
		client := &RfbClient{conn, bounds, mux, serv.regch, serv.unregch}
		go func() {
			defer conn.Close()
			handleConn(client, fbch)
		}()
	}
}

func decodePixelFormat(b [16]byte) PixelFormat {
	var f PixelFormat

	f.bpp = uint8(b[0])
	f.depth = uint8(b[1])
	f.beflag = uint8(b[2])
	f.trueColor = uint8(b[3])
	f.redMax = binary.BigEndian.Uint16(b[4:6])
	f.greenMax = binary.BigEndian.Uint16(b[6:8])
	f.blueMax = binary.BigEndian.Uint16(b[8:10])
	f.redShift = uint8(b[10])
	f.greenShift = uint8(b[11])
	f.blueShift = uint8(b[12])

	return f
}

func decodeServerStatus(b []byte) serverStatus {
	var c [16]byte
	w := binary.BigEndian.Uint16(b[0:2])
	h := binary.BigEndian.Uint16(b[2:4])
	copy(c[:], b[4:20])
	f := decodePixelFormat(c)
	s := string(b[24:])
	return serverStatus{int(w), int(h), f, s}
}

func decodeEncodings(b []byte) encodings {
	n := len(b) / 4
	e := make([]int32, n)
	for i := range e {
		e[i] = int32(binary.BigEndian.Uint32(b[4*i : 4*(i+1)]))
	}
	return e
}

func updateRequest(b [9]byte) updateRect {
	incr := b[0] == 1
	x := int(binary.BigEndian.Uint16(b[1:3]))
	y := int(binary.BigEndian.Uint16(b[3:5]))
	w := int(binary.BigEndian.Uint16(b[5:7]))
	h := int(binary.BigEndian.Uint16(b[7:9]))

	// Send the viewport of our remote client to the dirtyTracker goroutine.
	return updateRect{image.Rect(x, y, x+w, y+h), incr}
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

func (f PixelFormat) encode() []byte {
	b := make([]byte, 16)
	b[0] = byte(f.bpp)
	b[1] = byte(f.depth)
	b[2] = byte(f.beflag)
	b[3] = byte(f.trueColor)
	binary.BigEndian.PutUint16(b[4:6], f.redMax)
	binary.BigEndian.PutUint16(b[6:8], f.greenMax)
	binary.BigEndian.PutUint16(b[8:10], f.blueMax)
	b[10] = byte(f.redShift)
	b[11] = byte(f.greenShift)
	b[12] = byte(f.blueShift)
	return b
}

func (s serverStatus) encode() []byte {
	b := make([]byte, 24+len(s.name))
	binary.BigEndian.PutUint16(b[0:2], uint16(s.fbWidth))
	binary.BigEndian.PutUint16(b[2:4], uint16(s.fbHeight))
	copy(b[4:20], s.format.encode())
	binary.BigEndian.PutUint32(b[20:24], uint32(len(s.name)))
	copy(b[24:], s.name)
	return b
}

func (e encodings) encode() []byte {
	b := make([]byte, 4+4*len(e))
	b[0] = setEncodingsReq
	binary.BigEndian.PutUint16(b[2:4], uint16(len(e)))
	for i, j := range e {
		binary.BigEndian.PutUint32(b[4*i+4:4*i+8], uint32(j))
	}
	return b
}

func (rect updateRect) encode() []byte {
	b := make([]byte, 10)
	b[0] = byte(uint8(framebufferUpdateReq))
	if rect.incr {
		b[1] = 1
	} else {
		b[1] = 0
	}
	binary.BigEndian.PutUint16(b[2:4], uint16(rect.Min.X))
	binary.BigEndian.PutUint16(b[4:6], uint16(rect.Min.Y))
	binary.BigEndian.PutUint16(b[6:8], uint16(rect.Dx()))
	binary.BigEndian.PutUint16(b[8:10], uint16(rect.Dy()))
	return b
}

func (ev InputEvent) encode() []byte {
	var b []byte
	if ev.T == 0 {
		// Mouse Event
		b = make([]byte, 6)
		b[0] = byte(uint8(pointerEventReq))
		b[1] = byte(ev.Mask)
		binary.BigEndian.PutUint16(b[2:4], uint16(ev.Pos.X))
		binary.BigEndian.PutUint16(b[4:6], uint16(ev.Pos.Y))
	} else {
		// Keyboard Event
		b = make([]byte, 8)
		b[0] = byte(uint8(keyEventReq))
		b[1] = byte(ev.Mask)
		binary.BigEndian.PutUint32(b[4:8], ev.Key)
	}
	return b
}

func (ev CutEvent) encode() []byte {
	b := make([]byte, 8+len(ev.Txt))
	b[0] = byte(clientCutTextReq)
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

func rfbMux(ch <-chan muxMsg, serv *RfbServer) {
	state := rfbMuxState{serv.Input, serv.Txt}

	for msg := range ch {
		msg.work(&state)
	}
}

func rectHeader(rect image.Rectangle, encoding int32) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:2], uint16(rect.Min.X))
	binary.BigEndian.PutUint16(b[2:4], uint16(rect.Min.Y))
	binary.BigEndian.PutUint16(b[4:6], uint16(rect.Dx()))
	binary.BigEndian.PutUint16(b[6:8], uint16(rect.Dy()))
	binary.BigEndian.PutUint32(b[8:12], uint32(encoding))
	return b
}

func encodeRaw(img image.Image, rect image.Rectangle, b [][]byte) {
	x := rect.Min.X
	y := rect.Min.Y
	w := rect.Dx()
	h := rect.Dy()

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

	b[0] = rectHeader(rect, int32(encodingRaw))
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
	outbuf[0] = framebufferUpdateMsg
	outbuf[1] = 0 // padding
	binary.BigEndian.PutUint16(outbuf[2:4], uint16(nrects))
	outbytes[0] = outbuf
	for i, r := range rs {
		encodeRaw(img, r, outbytes[2*i+1:2*i+3])
	}
	return outbytes
}

func remove(ls []chan []image.Rectangle, a chan []image.Rectangle) []chan []image.Rectangle {
	res := ls
	for i, c := range ls {
		if a == c {
			res = append(ls[:i], ls[i+1:]...)
			close(a)
		}
	}
	return res
}

func updater(img draw.Image, fbch <-chan getUpdate, serv *RfbServer) {
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
				// Signal the d image.Rectangle to all the
				// dirtyTrackers. Avoid deadlock when any the
				// dirtyTracker wants to unregister.
				mylist := reglist[:]
				for {
					if len(mylist) == 0 {
						break
					}
					reg := mylist[0]
					select {
					case reg <- d:
						{
							mylist = mylist[1:]
						}
					case a := <-serv.unregch:
						{
							reglist = remove(reglist, a)
							mylist = remove(mylist, a)
						}
					}
				}
			}
		case a := <-fbch:
			{
				a.outch <- encodeDirty(img, a.Dirty)
			}
		case serv.regch <- ch:
			{
				reglist = append(reglist, ch)
				ch = make(chan []image.Rectangle)
			}
		case a := <-serv.unregch:
			{
				reglist = remove(reglist, a)
			}
		}
	}
}

func serve(port string, img draw.Image, serv *RfbServer) {
	muxch := make(chan muxMsg)
	defer close(muxch)
	fbch := make(chan getUpdate)
	defer close(fbch)

	go rfbMux(muxch, serv)
	go updater(img, fbch, serv)

	// go accepter(serv, muxch, fbch)
	accepter(serv, img.Bounds(), muxch, fbch)
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
	regch := make(chan chan []image.Rectangle)
	unregch := make(chan chan []image.Rectangle)

	serv := &RfbServer{ln, input, txt, getfb, relfb, regch, unregch}
	go serve(port, img, serv)

	return serv, nil
}

func ServeDumbFb(port string, w uint16, h uint16) (*RfbServer, error) {
	img := image.NewRGBA(image.Rect(0, 0, int(w), int(h)))
	black := color.RGBA{0, 0, 0, 0}
	draw.Draw(img, image.Rect(0, 0, int(w), int(h)), &image.Uniform{black}, image.ZP, draw.Src)

	serv, err := Server(port, img)
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
	close(serv.regch)
	close(serv.unregch)
}

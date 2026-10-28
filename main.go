// GridGuard demo: a minimal, dependency-free prototype.
//
//	sim     - simulated battery site speaking Modbus TCP
//	guard   - inline monitor that parses Modbus frames and raises alerts
//	client  - scripted scenarios: normal, bounds, reversal, source, export
//	demo    - runs everything in one process (best for screen recording)
//
// Simulation only. Run the scenarios against this local simulator only.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const (
	simAddr   = "127.0.0.1:5020" // simulated site
	guardAddr = "127.0.0.1:5021" // monitor, clients connect here

	regSOC      = 0 // state of charge, units of 0.1 %
	regSetpoint = 1 // power setpoint in kW, signed (+ charge, - discharge)
	regExport   = 2 // grid export limit in kW
	regPower    = 3 // reported power in kW, signed

	maxSetpointKW  = 50              // configured safe limit (placeholder)
	reversalWindow = 5 * time.Second // R3 window
	reversalLimit  = 2               // R3 flips inside the window
)

// R1 allowlist of command sources (placeholder).
var allowedSources = map[string]bool{"127.0.0.1": true}

// ---------- Modbus framing helpers ----------

// readFrame reads one Modbus TCP frame (7-byte MBAP header + PDU).
func readFrame(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 7)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[4:6])) // unit id + PDU
	if n < 2 || n > 260 {
		return nil, fmt.Errorf("bad frame length %d", n)
	}
	rest := make([]byte, n-1)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, err
	}
	return append(hdr, rest...), nil
}

var txID uint16

func req(pdu []byte) []byte {
	txID++
	f := make([]byte, 7, 7+len(pdu))
	binary.BigEndian.PutUint16(f[0:], txID)
	binary.BigEndian.PutUint16(f[4:], uint16(1+len(pdu)))
	f[6] = 1 // unit id
	return append(f, pdu...)
}

func writeReg(c net.Conn, addr uint16, v int16) error {
	pdu := []byte{0x06, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:], addr)
	binary.BigEndian.PutUint16(pdu[3:], uint16(v))
	if _, err := c.Write(req(pdu)); err != nil {
		return err
	}
	_, err := readFrame(c)
	return err
}

func readRegs(c net.Conn, addr, qty uint16) ([]uint16, error) {
	pdu := []byte{0x03, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(pdu[1:], addr)
	binary.BigEndian.PutUint16(pdu[3:], qty)
	if _, err := c.Write(req(pdu)); err != nil {
		return nil, err
	}
	f, err := readFrame(c)
	if err != nil {
		return nil, err
	}
	p := f[7:]
	if p[0] != 0x03 || len(p) < 2+int(qty)*2 {
		return nil, fmt.Errorf("bad read response")
	}
	out := make([]uint16, qty)
	for i := range out {
		out[i] = binary.BigEndian.Uint16(p[2+2*i:])
	}
	return out, nil
}

// ---------- Simulated site ----------

type device struct {
	mu   sync.Mutex
	regs [4]uint16
}

func newDevice() *device {
	d := &device{}
	d.regs[regSOC] = 500 // 50.0 %
	d.regs[regExport] = 100
	return d
}

func (d *device) get(a int) uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.regs[a]
}

func (d *device) set(a int, v uint16) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.regs[a] = v
}

// step advances the physics by one tick (1 tick = 1 simulated minute).
func (d *device) step() {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := int(int16(d.regs[regSetpoint]))
	soc := int(d.regs[regSOC]) + p/6 // 100 kWh battery, 0.1 % units
	if soc < 0 {
		soc = 0
	}
	if soc > 1000 {
		soc = 1000
	}
	d.regs[regSOC] = uint16(soc)
	d.regs[regPower] = d.regs[regSetpoint]
}

func runSim(verbose bool) {
	dev := newDevice()
	go func() {
		for range time.Tick(time.Second) {
			dev.step()
			if verbose {
				fmt.Printf("[SIM] SOC %.1f%%  setpoint %+d kW\n",
					float64(dev.get(regSOC))/10, int16(dev.get(regSetpoint)))
			}
		}
	}()
	ln, err := net.Listen("tcp", simAddr)
	if err != nil {
		fmt.Println("sim:", err)
		os.Exit(1)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go serve(c, dev)
	}
}

func serve(c net.Conn, dev *device) {
	defer c.Close()
	for {
		f, err := readFrame(c)
		if err != nil {
			return
		}
		pdu := f[7:]
		var resp []byte
		switch {
		case pdu[0] == 0x03 && len(pdu) >= 5: // read holding registers
			addr := int(binary.BigEndian.Uint16(pdu[1:3]))
			qty := int(binary.BigEndian.Uint16(pdu[3:5]))
			if qty < 1 || addr+qty > len(dev.regs) {
				resp = []byte{0x83, 0x02}
				break
			}
			resp = []byte{0x03, byte(qty * 2)}
			for i := 0; i < qty; i++ {
				v := dev.get(addr + i)
				resp = append(resp, byte(v>>8), byte(v))
			}
		case pdu[0] == 0x06 && len(pdu) >= 5: // write single register
			addr := int(binary.BigEndian.Uint16(pdu[1:3]))
			if addr >= len(dev.regs) {
				resp = []byte{0x86, 0x02}
				break
			}
			dev.set(addr, binary.BigEndian.Uint16(pdu[3:5]))
			resp = pdu
		default:
			resp = []byte{pdu[0] | 0x80, 0x01} // illegal function
		}
		out := make([]byte, 7, 7+len(resp))
		copy(out, f[:4])
		binary.BigEndian.PutUint16(out[4:], uint16(1+len(resp)))
		out[6] = f[6]
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}

// ---------- Guard: inline monitor ----------

type guard struct {
	mu       sync.Mutex
	lastSign int
	flips    []time.Time
	alerts   int
}

func (g *guard) alert(sev, id, msg string) { // caller holds g.mu
	g.alerts++
	fmt.Printf("\033[31m[ALERT][%s] %s %s\033[0m\n", sev, id, msg)
}

func (g *guard) checkWrite(src string, addr uint16, v int16) {
	g.mu.Lock()
	defer g.mu.Unlock()
	flagged := false

	if !allowedSources[src] { // R1
		g.alert("HIGH", "R1 unauthorised source:", fmt.Sprintf("%s wrote register %d", src, addr))
		flagged = true
	}

	switch addr {
	case regSetpoint:
		kw := int(v)
		if kw > maxSetpointKW || kw < -maxSetpointKW { // R2
			g.alert("HIGH", "R2 out-of-bounds setpoint:",
				fmt.Sprintf("%+d kW, safe limit is +/-%d kW (from %s)", kw, maxSetpointKW, src))
			flagged = true
		}
		sign := 0
		if kw > 0 {
			sign = 1
		} else if kw < 0 {
			sign = -1
		}
		if sign != 0 {
			if g.lastSign != 0 && sign != g.lastSign { // R3
				now := time.Now()
				g.flips = append(g.flips, now)
				kept := g.flips[:0]
				for _, t := range g.flips {
					if now.Sub(t) <= reversalWindow {
						kept = append(kept, t)
					}
				}
				g.flips = kept
				if len(g.flips) >= reversalLimit {
					g.alert("MEDIUM", "R3 rapid charge/discharge reversal:",
						fmt.Sprintf("%d direction changes within %s", len(g.flips), reversalWindow))
					flagged = true
				}
			}
			g.lastSign = sign
		}
	case regExport: // R7
		g.alert("MEDIUM", "R7 export limit changed:", fmt.Sprintf("new value %d kW (from %s)", v, src))
		flagged = true
	default: // write to a telemetry register
		g.alert("HIGH", "R8 write to read-only telemetry register:", fmt.Sprintf("register %d (from %s)", addr, src))
		flagged = true
	}

	if !flagged {
		fmt.Printf("[GUARD] write ok: register %d = %d from %s\n", addr, v, src)
	}
}

func (g *guard) inspect(f []byte, src string) {
	pdu := f[7:]
	switch pdu[0] {
	case 0x03: // reads are not inspected in this demo
	case 0x06:
		if len(pdu) >= 5 {
			g.checkWrite(src, binary.BigEndian.Uint16(pdu[1:3]), int16(binary.BigEndian.Uint16(pdu[3:5])))
		}
	default:
		g.mu.Lock()
		g.alert("MEDIUM", "R0 unexpected function code:", fmt.Sprintf("0x%02x from %s", pdu[0], src))
		g.mu.Unlock()
	}
}

func (g *guard) handle(client net.Conn) {
	defer client.Close()
	sim, err := net.Dial("tcp", simAddr)
	if err != nil {
		fmt.Println("[GUARD] cannot reach simulator:", err)
		return
	}
	defer sim.Close()
	src, _, _ := net.SplitHostPort(client.RemoteAddr().String())
	go io.Copy(client, sim) // responses pass straight through
	for {
		f, err := readFrame(client)
		if err != nil {
			return
		}
		g.inspect(f, src)
		if _, err := sim.Write(f); err != nil { // detect only, never blocks
			return
		}
	}
}

func runGuard(g *guard) {
	ln, err := net.Listen("tcp", guardAddr)
	if err != nil {
		fmt.Println("guard:", err)
		os.Exit(1)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go g.handle(c)
	}
}

// ---------- Scenarios ----------

type w struct {
	addr uint16
	v    int16
}

func scenario(name string) error {
	src := ""
	var seq []w
	switch name {
	case "normal":
		seq = []w{{regSetpoint, 20}, {regSetpoint, 30}, {regSetpoint, 25}}
	case "bounds":
		seq = []w{{regSetpoint, 250}}
	case "reversal":
		seq = []w{{regSetpoint, 30}, {regSetpoint, -30}, {regSetpoint, 30}, {regSetpoint, -30}, {regSetpoint, 30}}
	case "source":
		src = "127.0.0.2" // Linux: all of 127.0.0.0/8 is local
		seq = []w{{regSetpoint, 10}}
	case "export":
		seq = []w{{regExport, 1000}}
	default:
		return fmt.Errorf("unknown scenario %q (normal, bounds, reversal, source, export)", name)
	}

	d := net.Dialer{Timeout: 2 * time.Second}
	if src != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(src)}
	}
	c, err := d.Dial("tcp", guardAddr)
	if err != nil {
		return err
	}
	defer c.Close()

	for _, s := range seq {
		fmt.Printf("[CLIENT] write register %d = %d\n", s.addr, s.v)
		if err := writeReg(c, s.addr, s.v); err != nil {
			return err
		}
		time.Sleep(700 * time.Millisecond)
	}
	if regs, err := readRegs(c, 0, 4); err == nil {
		fmt.Printf("[CLIENT] site state: SOC %.1f%%, setpoint %d kW, export limit %d kW\n",
			float64(regs[0])/10, int16(regs[1]), regs[2])
	}
	return nil
}

func demo() {
	g := &guard{}
	go runSim(false)
	go runGuard(g)
	time.Sleep(500 * time.Millisecond)

	steps := []struct{ title, name string }{
		{"1. Normal operation (expect no alerts)", "normal"},
		{"2. Attack: out-of-bounds setpoint", "bounds"},
		{"3. Attack: rapid charge/discharge reversal", "reversal"},
		{"4. Attack: unauthorised source", "source"},
		{"5. Attack: export limit tampering", "export"},
	}
	for _, s := range steps {
		fmt.Printf("\n\033[1m%s\033[0m\n", s.title)
		if err := scenario(s.name); err != nil {
			fmt.Println("scenario error:", err)
		}
		time.Sleep(time.Second)
	}
	g.mu.Lock()
	n := g.alerts
	g.mu.Unlock()
	fmt.Printf("\nDone. %d alerts raised across 4 attack scenarios.\n", n)
}

func usage() {
	fmt.Println("usage: go run . <sim | guard | client <scenario> | demo>")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "sim":
		runSim(true)
	case "guard":
		runGuard(&guard{})
	case "client":
		if len(os.Args) < 3 {
			usage()
			return
		}
		if err := scenario(os.Args[2]); err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
	case "demo":
		demo()
	default:
		usage()
	}
}

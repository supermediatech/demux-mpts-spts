package main

// Duplicate rules. The database already rejects identical URLs; these also
// catch the same stream written differently:
//
//   - Two inputs are the same stream when they receive the same group:port
//     (and SSM source), whatever the protocol, on the same interface. An
//     input without an interface ("any") overlaps every interface. Different
//     explicit interfaces are allowed (main / backup networks often carry
//     the same group).
//   - Two outputs clash when they send to the same group:port on overlapping
//     interfaces, and an output may not send to an address an input receives
//     (it would mix into that input's stream).
//   - A program (input + service ID) gets at most one output per egress
//     interface (localaddr); "" (default route) counts as its own value.

import (
	"fmt"
	"net/netip"
)

func ifaceName(la string) string {
	if la == "" {
		return "the default route"
	}
	return "interface " + la
}

func ifaceOverlap(x, y string) bool { return x == y || x == "" || y == "" }

func sameInput(a, b StreamURL) bool {
	return a.Addr == b.Addr && a.Source == b.Source && ifaceOverlap(a.LocalAddr, b.LocalAddr)
}

func sameDest(a, b StreamURL) bool {
	return a.Addr == b.Addr && ifaceOverlap(a.LocalAddr, b.LocalAddr)
}

// inputClash returns the existing input (other than self) that receives the
// same stream as su.
func (a *App) inputClash(su StreamURL, self int64) (*Input, error) {
	ins, err := a.store.Inputs()
	if err != nil {
		return nil, err
	}
	for i := range ins {
		if ins[i].ID == self {
			continue
		}
		if cu, err := ParseStreamURL(ins[i].URL); err == nil && sameInput(su, cu) {
			return &ins[i], nil
		}
	}
	return nil, nil
}

// outputClash checks a new or edited output (self = its ID, 0 when new).
func (a *App) outputClash(o Output, su StreamURL, self int64) error {
	outs, err := a.store.Outputs()
	if err != nil {
		return err
	}
	for _, x := range outs {
		if x.ID == self {
			continue
		}
		cu, err := ParseStreamURL(x.URL)
		if err != nil {
			continue
		}
		if x.InputID == o.InputID && x.SrcSID == o.SrcSID && cu.LocalAddr == su.LocalAddr {
			return fmt.Errorf("this program already has an output on %s: %q → %s", ifaceName(su.LocalAddr), x.Name, x.URL)
		}
		if sameDest(su, cu) {
			return fmt.Errorf("%s is already used by output %q (%s)", su.Addr, x.Name, x.URL)
		}
	}
	ins, err := a.store.Inputs()
	if err != nil {
		return err
	}
	for _, in := range ins {
		if cu, err := ParseStreamURL(in.URL); err == nil && sameDest(su, cu) {
			return fmt.Errorf("%s is received by input %q; an output there would mix into it", su.Addr, in.Name)
		}
	}
	return nil
}

// inputAddrs lists the addresses inputs receive, so automatic output
// addresses never land on them.
func (a *App) inputAddrs() map[netip.AddrPort]bool {
	m := map[netip.AddrPort]bool{}
	ins, _ := a.store.Inputs()
	for _, in := range ins {
		if su, err := ParseStreamURL(in.URL); err == nil {
			m[su.Addr] = true
		}
	}
	return m
}

// dedupe drops near-duplicate entries from a validated backup (made before
// these rules existed): a duplicate input's outputs move to the input kept,
// then outputs that repeat a program or a destination are dropped.
func (b *Backup) dedupe() (skippedIn, skippedOut int) {
	type kept struct {
		id int64
		su StreamURL
	}
	var ks []kept
	remap := map[int64]int64{}
	ins := b.Inputs[:0]
	for _, in := range b.Inputs {
		su, _ := ParseStreamURL(in.URL)
		dup := false
		for _, k := range ks {
			if sameInput(su, k.su) {
				remap[in.ID], dup = k.id, true
				break
			}
		}
		if dup {
			skippedIn++
			continue
		}
		ks = append(ks, kept{in.ID, su})
		ins = append(ins, in)
	}
	b.Inputs = ins
	var dests []StreamURL
	type prog struct {
		in  int64
		sid int
		la  string
	}
	progs := map[prog]bool{}
	outs := b.Outputs[:0]
	for _, o := range b.Outputs {
		if id, ok := remap[o.InputID]; ok {
			o.InputID = id
		}
		su, _ := ParseStreamURL(o.URL)
		dup := progs[prog{o.InputID, o.SrcSID, su.LocalAddr}]
		for _, d := range dests {
			dup = dup || sameDest(su, d)
		}
		for _, k := range ks {
			dup = dup || sameDest(su, k.su)
		}
		if dup {
			skippedOut++
			continue
		}
		progs[prog{o.InputID, o.SrcSID, su.LocalAddr}] = true
		dests = append(dests, su)
		outs = append(outs, o)
	}
	b.Outputs = outs
	return
}

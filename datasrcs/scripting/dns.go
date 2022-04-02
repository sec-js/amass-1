// Copyright © by Jeff Foley 2021-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package scripting

import (
	"context"
	"errors"
	"strings"

	"github.com/caffix/resolve"
	"github.com/miekg/dns"
	lua "github.com/yuin/gopher-lua"
	"golang.org/x/net/publicsuffix"
)

// Wrapper so that scripts can make DNS queries.
func (s *Script) resolve(L *lua.LState) int {
	ctx, err := extractContext(L.CheckUserData(1))
	name := L.CheckString(2)
	qtype := convertType(L.CheckString(3))
	if err != nil || name == "" || qtype == 0 {
		L.Push(lua.LNil)
		L.Push(lua.LString("Proper parameters were not provided"))
		return 2
	}

	resp, err := s.fwdQuery(ctx, name, qtype)
	if err != nil || resp.Rcode != dns.RcodeSuccess || len(resp.Answer) == 0 {
		L.Push(lua.LNil)
		L.Push(lua.LString("The query was unsuccessful for " + name))
		return 2
	}

	detection := true
	if L.GetTop() == 4 {
		detection = L.CheckBool(4)
	}

	if detection {
		domain, err := publicsuffix.EffectiveTLDPlusOne(name)

		if err != nil || s.sys.TrustedResolvers().WildcardDetected(ctx, resp, domain) {
			L.Push(lua.LNil)
			L.Push(lua.LString("DNS wildcard detection made a positive match for " + name))
			return 2
		}
	}

	tb := L.NewTable()
	if ans := resolve.ExtractAnswers(resp); len(ans) > 0 {
		if records := resolve.AnswersByType(ans, qtype); len(records) > 0 {
			for _, rr := range records {
				entry := L.NewTable()
				entry.RawSetString("rrname", lua.LString(rr.Name))
				entry.RawSetString("rrtype", lua.LNumber(rr.Type))
				entry.RawSetString("rrdata", lua.LString(rr.Data))
				tb.Append(entry)
			}
		}
	}
	L.Push(tb)
	L.Push(lua.LNil)
	return 2
}

func (s *Script) fwdQuery(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	msg := resolve.QueryMsg(name, qtype)
	resp, err := s.sys.Resolvers().QueryBlocking(ctx, msg)
	// Check if the response indicates that the name does not exist
	if err != nil || resp.Rcode == dns.RcodeNameError {
		return nil, errors.New("name does not exist")
	}
	if resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 0 {
		return nil, errors.New("zero answers returned")
	}
	// Was there another reason why the query failed?
	for attempts := 1; attempts < 50 && resp.Rcode != dns.RcodeSuccess; attempts++ {
		resp, err = s.sys.Resolvers().QueryBlocking(ctx, msg)
		// Check if the response indicates that the name does not exist
		if err != nil || resp.Rcode == dns.RcodeNameError {
			return nil, errors.New("name does not exist")
		}
		if resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 0 {
			return nil, errors.New("zero answers returned")
		}
	}
	if resp.Rcode != dns.RcodeSuccess {
		return nil, errors.New("query failed")
	}

	resp, err = s.sys.TrustedResolvers().QueryBlocking(ctx, msg)
	// Check if the response indicates that the name does not exist
	if err != nil || resp.Rcode == dns.RcodeNameError {
		return nil, errors.New("name does not exist")
	}
	if resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 0 {
		return nil, errors.New("zero answers returned")
	}
	for attempts := 1; attempts < 50 && resp.Rcode != dns.RcodeSuccess; attempts++ {
		resp, err = s.sys.Resolvers().QueryBlocking(ctx, msg)
		// Check if the response indicates that the name does not exist
		if err != nil || resp.Rcode == dns.RcodeNameError {
			return nil, errors.New("name does not exist")
		}
		if resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 0 {
			return nil, errors.New("zero answers returned")
		}
	}
	if resp.Rcode != dns.RcodeSuccess {
		return nil, errors.New("query failed")
	}
	return resp, nil
}

func convertType(qtype string) uint16 {
	var t uint16

	switch strings.ToLower(qtype) {
	case "a":
		t = dns.TypeA
	case "aaaa":
		t = dns.TypeAAAA
	case "cname":
		t = dns.TypeCNAME
	case "ptr":
		t = dns.TypePTR
	case "ns":
		t = dns.TypeNS
	case "mx":
		t = dns.TypeMX
	case "txt":
		t = dns.TypeTXT
	case "soa":
		t = dns.TypeSOA
	case "srv":
		t = dns.TypeSRV
	}
	return t
}

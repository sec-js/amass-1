// Copyright © by Jeff Foley 2021-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package datasrcs

import (
	"context"
	"errors"
	"fmt"

	"github.com/OWASP/Amass/v3/config"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/systems"
	"github.com/caffix/service"
	"github.com/fofapro/fofa-go/fofa"
)

// FOFA is the Service that handles access to the FOFA data source.
type FOFA struct {
	service.BaseService

	SourceType string
	sys        systems.System
	creds      *config.Credentials
}

// NewFOFA returns he object initialized, but not yet started.
func NewFOFA(sys systems.System) *FOFA {
	f := &FOFA{
		SourceType: requests.SCRAPE,
		sys:        sys,
	}

	go f.requests()
	f.BaseService = *service.NewBaseService(f, "FOFA")
	return f
}

// Description implements the Service interface.
func (f *FOFA) Description() string {
	return f.SourceType
}

// OnStart implements the Service interface.
func (f *FOFA) OnStart() error {
	f.creds = f.sys.Config().GetDataSourceConfig(f.String()).GetCredentials()

	if f.creds == nil || f.creds.Username == "" || f.creds.Key == "" {
		estr := fmt.Sprintf("%s: Email address or API key data was not provided", f.String())

		f.sys.Config().Log.Print(estr)
		return errors.New(estr)
	}

	f.SetRateLimit(1)
	return nil
}

func (f *FOFA) requests() {
	for {
		select {
		case <-f.Done():
			return
		case in := <-f.Input():
			switch req := in.(type) {
			case *requests.DNSRequest:
				f.CheckRateLimit()
				f.dnsRequest(context.TODO(), req)
			}
		}
	}
}

func (f *FOFA) dnsRequest(ctx context.Context, req *requests.DNSRequest) {
	if f.creds == nil || f.creds.Username == "" || f.creds.Key == "" {
		return
	}

	if !f.sys.Config().IsDomainInScope(req.Domain) {
		return
	}

	f.sys.Config().Log.Printf("Querying %s for %s subdomains", f.String(), req.Domain)

	client := fofa.NewFofaClient([]byte(f.creds.Username), []byte(f.creds.Key))
	if client == nil {
		f.sys.Config().Log.Printf("%s: Failed to create FOFA client", f.String())
		return
	}

	for i := 1; i <= 10; i++ {
		results, err := client.QueryAsArray(uint(i), []byte(fmt.Sprintf("domain=\"%s\"", req.Domain)), []byte("domain"))
		if err != nil {
			f.sys.Config().Log.Printf("%s: %v", f.String(), err)
			return
		}
		if len(results) == 0 {
			break
		}

		for _, res := range results {
			genNewNameEvent(ctx, f.sys, f, res.Domain)
		}
		f.CheckRateLimit()
	}
}

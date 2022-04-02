// Copyright © by Jeff Foley 2017-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package datasrcs

import (
	"context"
	"sort"

	"github.com/OWASP/Amass/v3/config"
	"github.com/OWASP/Amass/v3/datasrcs/scripting"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/systems"
	"github.com/caffix/service"
	"github.com/caffix/stringset"
)

// GetAllSources returns a slice of all data source services, initialized and ready.
func GetAllSources(sys systems.System) []service.Service {
	srvs := []service.Service{
		NewAlienVault(sys),
		NewCloudflare(sys),
		NewDNSDB(sys),
		NewFOFA(sys),
		NewNetworksDB(sys),
		NewRADb(sys),
		NewTwitter(sys),
		NewUmbrella(sys),
	}

	if scripts, err := sys.Config().AcquireScripts(); err == nil {
		for _, script := range scripts {
			if s := scripting.NewScript(script, sys); s != nil {
				srvs = append(srvs, s)
			}
		}
	}

	sort.Slice(srvs, func(i, j int) bool {
		return srvs[i].String() < srvs[j].String()
	})
	return srvs
}

// SelectedDataSources uses the config and available data sources to return the selected data sources.
func SelectedDataSources(cfg *config.Config, avail []service.Service) []service.Service {
	specified := stringset.New()
	defer specified.Close()
	specified.InsertMany(cfg.SourceFilter.Sources...)

	available := stringset.New()
	defer available.Close()
	for _, src := range avail {
		available.Insert(src.String())
	}

	if specified.Len() > 0 && cfg.SourceFilter.Include {
		available.Intersect(specified)
	} else {
		available.Subtract(specified)
	}

	var results []service.Service
	for _, src := range avail {
		if available.Has(src.String()) {
			results = append(results, src)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].String() < results[j].String()
	})
	return results
}

func genNewNameEvent(ctx context.Context, sys systems.System, srv service.Service, name string) {
	if domain := sys.Config().WhichDomain(name); domain != "" {
		srv.Output() <- &requests.DNSRequest{
			Name:   name,
			Domain: domain,
			Tag:    srv.Description(),
			Source: srv.String(),
		}
	}
}

func numRateLimitChecks(srv service.Service, num int) {
	for i := 0; i < num; i++ {
		srv.CheckRateLimit()
	}
}

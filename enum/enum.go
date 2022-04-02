// Copyright © by Jeff Foley 2017-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package enum

import (
	"context"
	"strconv"
	"sync"

	"github.com/OWASP/Amass/v3/config"
	"github.com/OWASP/Amass/v3/datasrcs"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/systems"
	"github.com/caffix/netmap"
	"github.com/caffix/pipeline"
	"github.com/caffix/queue"
	"github.com/caffix/service"
	bf "github.com/tylertreat/BoomFilters"
)

const maxActivePipelineTasks int = 25

// Enumeration is the object type used to execute a DNS enumeration.
type Enumeration struct {
	Config   *config.Config
	Sys      systems.System
	ctx      context.Context
	graph    *netmap.Graph
	srcs     []service.Service
	done     chan struct{}
	nameSrc  *enumSource
	subTask  *subdomainTask
	dnsTask  *dnsTask
	store    *dataManager
	requests queue.Queue
}

// NewEnumeration returns an initialized Enumeration that has not been started yet.
func NewEnumeration(cfg *config.Config, sys systems.System, graph *netmap.Graph) *Enumeration {
	return &Enumeration{
		Config:   cfg,
		Sys:      sys,
		graph:    graph,
		srcs:     datasrcs.SelectedDataSources(cfg, sys.DataSources()),
		requests: queue.NewQueue(),
	}
}

// Start begins the vertical domain correlation process.
func (e *Enumeration) Start(ctx context.Context) error {
	e.done = make(chan struct{})
	defer close(e.done)

	if err := e.Config.CheckSettings(); err != nil {
		return err
	}
	// This context, used throughout the enumeration, will provide the
	// ability to pass the configuration and event bus to all the components
	var cancel context.CancelFunc
	e.ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	go e.manageDataSrcRequests()

	if !e.Config.Passive {
		e.dnsTask = newDNSTask(e)
		e.store = newDataManager(e)
		e.subTask = newSubdomainTask(e)
		defer e.subTask.Stop()
	}
	// The pipeline input source will receive all the names
	e.nameSrc = newEnumSource(e)
	defer e.nameSrc.Stop()

	var stages []pipeline.Stage
	if !e.Config.Passive {
		stages = append(stages, pipeline.FIFO("root", e.dnsTask.rootTaskFunc()))
		stages = append(stages, pipeline.DynamicPool("dns", e.dnsTask, e.Sys.Resolvers().QPS()))
		stages = append(stages, pipeline.FIFO("store", e.store))
		stages = append(stages, pipeline.FIFO("", e.subTask))
	} else {
		filter := bf.NewDefaultStableBloomFilter(1000000, 0.01)
		defer func() { _ = filter.Reset() }()
		stages = append(stages, pipeline.FIFO("filter", e.filterTaskFunc(filter)))
	}
	if e.Config.Active {
		activetask := newActiveTask(e, maxActivePipelineTasks)
		defer activetask.Stop()
		stages = append(stages, pipeline.FIFO("active", activetask))
	}

	e.submitASNs()
	e.submitDomainNames()
	/*
	 * Now that the pipeline input source has been setup, names provided
	 * by the user and names acquired from the graph database can be brought
	 * into the enumeration
	 */
	var wg sync.WaitGroup
	wg.Add(2)
	go e.submitKnownNames(&wg)
	go e.submitProvidedNames(&wg)
	wg.Wait()

	var err error
	if p := pipeline.NewPipeline(stages...); e.Config.Passive {
		err = p.Execute(e.ctx, e.nameSrc, e.makeOutputSink())
	} else {
		err = p.ExecuteBuffered(e.ctx, e.nameSrc, e.makeOutputSink(), 50)
		// Ensure all data has been stored
		<-e.store.Stop()
	}
	return err
}

// Release the root domain names to the input source and each data source.
func (e *Enumeration) submitDomainNames() {
	for _, domain := range e.Config.Domains() {
		req := &requests.DNSRequest{
			Name:   domain,
			Domain: domain,
			Tag:    requests.DNS,
			Source: "DNS",
		}

		e.nameSrc.newName(req)
		e.sendRequests(req.Clone().(*requests.DNSRequest))
	}
}

// If requests were made for specific ASNs, then those requests are
// sent to included data sources at this point.
func (e *Enumeration) submitASNs() {
	for _, asn := range e.Config.ASNs {
		e.sendRequests(&requests.ASNRequest{ASN: asn})
	}
}

func (e *Enumeration) sendRequests(element interface{}) {
	e.requests.Append(element)
}

func (e *Enumeration) manageDataSrcRequests() {
	nameToSrc := make(map[string]service.Service)
	for _, src := range e.srcs {
		nameToSrc[src.String()] = src
	}

	pending := make(map[string]bool)
	for _, src := range e.srcs {
		pending[src.String()] = false
	}

	finished := make(chan string, len(e.srcs))
	requestsMap := make(map[string][]interface{})
loop:
	for {
		select {
		case <-e.done:
			break loop
		case <-e.ctx.Done():
			break loop
		case <-e.requests.Signal():
			element, ok := e.requests.Next()
			if !ok {
				continue loop
			}
			for name := range nameToSrc {
				if len(requestsMap[name]) == 0 && !pending[name] {
					go e.fireRequest(nameToSrc[name], element, finished)
					pending[name] = true
				} else {
					requestsMap[name] = append(requestsMap[name], element)
				}
			}
		case name := <-finished:
			if len(requestsMap[name]) == 0 {
				pending[name] = false
				continue loop
			}

			go e.fireRequest(nameToSrc[name], requestsMap[name][0], finished)
			requestsMap[name] = requestsMap[name][1:]
		}
	}
	e.requests.Process(func(e interface{}) {})
}

func (e *Enumeration) fireRequest(srv service.Service, req interface{}, finished chan string) {
	select {
	case <-e.done:
	case <-e.ctx.Done():
	case <-srv.Done():
	case srv.Input() <- req:
	}
	finished <- srv.String()
}

func (e *Enumeration) makeOutputSink() pipeline.SinkFunc {
	return pipeline.SinkFunc(func(ctx context.Context, data pipeline.Data) error {
		if !e.Config.Passive {
			return nil
		}

		req, ok := data.(*requests.DNSRequest)
		if ok && req != nil && req.Name != "" && e.Config.IsDomainInScope(req.Name) {
			if _, err := e.graph.UpsertFQDN(e.ctx, req.Name, req.Source, e.Config.UUID.String()); err != nil {
				e.Config.Log.Print(err.Error())
			}
		}
		return nil
	})
}

func (e *Enumeration) filterTaskFunc(filter *bf.StableBloomFilter) pipeline.TaskFunc {
	return pipeline.TaskFunc(func(ctx context.Context, data pipeline.Data, tp pipeline.TaskParams) (pipeline.Data, error) {
		select {
		case <-ctx.Done():
			return nil, nil
		default:
		}

		var name, qtype string
		switch v := data.(type) {
		case *requests.DNSRequest:
			if v != nil && v.Valid() {
				name = v.Name
				if len(v.Records) > 0 {
					qtype = strconv.Itoa(v.Records[0].Type)
				}
			}
		case *requests.AddrRequest:
			if v != nil && v.Valid() {
				name = v.Address
			}
		default:
			return data, nil
		}

		if name != "" && !filter.TestAndAdd([]byte(name+qtype)) {
			return data, nil
		}
		return nil, nil
	})
}

func (e *Enumeration) submitKnownNames(wg *sync.WaitGroup) {
	defer wg.Done()

	filter := bf.NewDefaultStableBloomFilter(1000000, 0.01)
	defer func() { _ = filter.Reset() }()

	srcTags := make(map[string]string)
	for _, src := range e.Sys.DataSources() {
		srcTags[src.String()] = src.Description()
	}

	for _, g := range e.Sys.GraphDatabases() {
		e.readNamesFromDatabase(e.ctx, g, filter, srcTags)
	}
}

func (e *Enumeration) readNamesFromDatabase(ctx context.Context, g *netmap.Graph, filter *bf.StableBloomFilter, stags map[string]string) {
	domains := e.Config.Domains()

	for _, event := range g.EventsInScope(ctx, domains...) {
		for _, name := range g.EventFQDNs(ctx, event) {
			select {
			case <-e.done:
				return
			default:
			}

			if filter.TestAndAdd([]byte(name)) {
				continue
			}

			domain := e.Config.WhichDomain(name)
			if domain == "" {
				continue
			}
			if srcs, err := g.NodeSources(ctx, netmap.Node(name), event); err == nil {
				src := srcs[0]
				tag := stags[src]
				req := &requests.DNSRequest{
					Name:   name,
					Domain: domain,
					Tag:    tag,
					Source: src,
				}

				if e.Config.IsDomainInScope(req.Name) {
					e.nameSrc.newName(req)
				}
			}
		}
	}
}

func (e *Enumeration) submitProvidedNames(wg *sync.WaitGroup) {
	defer wg.Done()

	for _, name := range e.Config.ProvidedNames {
		if domain := e.Config.WhichDomain(name); domain != "" {
			req := &requests.DNSRequest{
				Name:   name,
				Domain: domain,
				Tag:    requests.EXTERNAL,
				Source: "User Input",
			}

			if e.Config.IsDomainInScope(req.Name) {
				e.nameSrc.newName(req)
			}
		}
	}
}

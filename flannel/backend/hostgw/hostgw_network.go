// Copyright 2015 flannel authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hostgw

import (
	"bytes"
	"net"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"

	"github.com/coreos/flannel/backend"
	"github.com/coreos/flannel/subnet"
)

type network struct {
	name     string
	extIface *backend.ExternalInterface
	rl       []netlink.Route
	lease    *subnet.Lease
	sm       subnet.Manager
}

func (n *network) Lease() *subnet.Lease {
	return n.lease
}

func (n *network) MTU() int {
	return n.extIface.Iface.MTU
}

func (n *network) LinkIndex() int {
	return n.extIface.Iface.Index
}

func (n *network) Run(ctx context.Context) {
	wg := sync.WaitGroup{}

	log.Info("Watching for new subnet leases")
	evts := make(chan []subnet.Event)
	wg.Add(1)
	go func() {
		// 最终调用subnet manager的watch lease，获得关于lease的event
		subnet.WatchLeases(ctx, n.sm, n.lease, evts)
		wg.Done()
	}()

	// Store a list of routes, initialized to capacity of 10.
	n.rl = make([]netlink.Route, 0, 10)
	wg.Add(1)

	// Start a goroutine which periodically checks that the right routes are created
	go func() {
		n.routeCheck(ctx)
		wg.Done()
	}()

	defer wg.Wait()

	for {
		select {
		case evtBatch := <-evts:
			// 获取lease event，交由handleSubnetEvents进行具体的处理
			n.handleSubnetEvents(evtBatch)

		case <-ctx.Done():
			return
		}
	}
}

func (n *network) handleSubnetEvents(batch []subnet.Event) {
	for _, evt := range batch {
		switch evt.Type {
		// 获取添加subnet的event
		case subnet.EventAdded:
			log.Infof("Subnet added: %v via %v", evt.Lease.Subnet, evt.Lease.Attrs.PublicIP)

			if evt.Lease.Attrs.BackendType != "host-gw" {
				log.Warningf("Ignoring non-host-gw subnet: type=%v", evt.Lease.Attrs.BackendType)
				continue
			}

			// 添加通往新增加的subnet的路由
			route := netlink.Route{
				Dst:       evt.Lease.Subnet.ToIPNet(),
				Gw:        evt.Lease.Attrs.PublicIP.ToIP(),
				LinkIndex: n.LinkIndex(),
			}

			// Always add the route to the route list.
			// 将route加入加入network的rl数组
			n.addToRouteList(route)

			// Check if route exists before attempting to add it
			routeList, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{
				Dst: route.Dst,
			}, netlink.RT_FILTER_DST)
			if err != nil {
				log.Warningf("Unable to list routes: %v", err)
			}
			// Check match on Dst for match on Gw
			// 为什么都只判断routeList中的第一个元素？
			if len(routeList) > 0 && !routeList[0].Gw.Equal(route.Gw) {
				// Same Dst different Gw. Remove it, correct route will be added below.
				// 将Dst相同，但是Gw不同的路由删除
				log.Warningf("Replacing existing route to %v via %v with %v via %v.", evt.Lease.Subnet, routeList[0].Gw, evt.Lease.Subnet, evt.Lease.Attrs.PublicIP)
				if err := netlink.RouteDel(&routeList[0]); err != nil {
					log.Errorf("Error deleting route to %v: %v", evt.Lease.Subnet, err)
					continue
				}
			}
			if len(routeList) > 0 && routeList[0].Gw.Equal(route.Gw) {
				// Same Dst and same Gw, keep it and do not attempt to add it.
				// 如果路由的Dst和Gw都相同的话，则没有必要进行任何的修改
				log.Infof("Route to %v via %v already exists, skipping.", evt.Lease.Subnet, evt.Lease.Attrs.PublicIP)
			} else if err := netlink.RouteAdd(&route); err != nil {
				log.Errorf("Error adding route to %v via %v: %v", evt.Lease.Subnet, evt.Lease.Attrs.PublicIP, err)
				continue
			}

		case subnet.EventRemoved:
			log.Info("Subnet removed: ", evt.Lease.Subnet)

			if evt.Lease.Attrs.BackendType != "host-gw" {
				log.Warningf("Ignoring non-host-gw subnet: type=%v", evt.Lease.Attrs.BackendType)
				continue
			}

			route := netlink.Route{
				Dst:       evt.Lease.Subnet.ToIPNet(),
				Gw:        evt.Lease.Attrs.PublicIP.ToIP(),
				LinkIndex: n.LinkIndex(),
			}

			// Always remove the route from the route list.
			// 先从network的rl数组中删除
			n.removeFromRouteList(route)

			if err := netlink.RouteDel(&route); err != nil {
				log.Errorf("Error deleting route to %v: %v", evt.Lease.Subnet, err)
				continue
			}

		default:
			log.Error("Internal error: unknown event type: ", int(evt.Type))
		}
	}
}

func (n *network) addToRouteList(route netlink.Route) {
	for _, r := range n.rl {
		if routeEqual(r, route) {
			return
		}
	}
	n.rl = append(n.rl, route)
}

func (n *network) removeFromRouteList(route netlink.Route) {
	for index, r := range n.rl {
		if routeEqual(r, route) {
			n.rl = append(n.rl[:index], n.rl[index+1:]...)
			return
		}
	}
}

func (n *network) routeCheck(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(routeCheckRetries * time.Second):
			n.checkSubnetExistInRoutes()
		}
	}
}

// checkSubnetExistInRoutes检查network中的路由集合rl是否都存在在主机的路由表中
func (n *network) checkSubnetExistInRoutes() {
	routeList, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err == nil {
		for _, route := range n.rl {
			exist := false
			for _, r := range routeList {
				if r.Dst == nil {
					continue
				}
				if routeEqual(r, route) {
					exist = true
					break
				}
			}
			if !exist {
				// 恢复network的rl中没添加到主机中的路由
				if err := netlink.RouteAdd(&route); err != nil {
					if nerr, ok := err.(net.Error); !ok {
						log.Errorf("Error recovering route to %v: %v, %v", route.Dst, route.Gw, nerr)
					}
					continue
				} else {
					log.Infof("Route recovered %v : %v", route.Dst, route.Gw)
				}
			}
		}
	} else {
		log.Errorf("Error fetching route list. Will automatically retry: %v", err)
	}
}

func routeEqual(x, y netlink.Route) bool {
	if x.Dst.IP.Equal(y.Dst.IP) && x.Gw.Equal(y.Gw) && bytes.Equal(x.Dst.Mask, y.Dst.Mask) {
		return true
	}
	return false
}

package hcsoci

import (
	"context"

	"github.com/Microsoft/hcsshim/internal/hns"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/logfields"
	"github.com/Microsoft/hcsshim/internal/resources"
	"github.com/Microsoft/hcsshim/internal/uvm"
	"github.com/sirupsen/logrus"
)

func createNetworkNamespace(ctx context.Context, coi *createOptionsInternal, r *resources.Resources) error {
	op := "hcsoci::createNetworkNamespace"
	l := log.G(ctx).WithField(logfields.ContainerID, coi.ID)
	l.Debug(op + " - Begin")
	defer func() {
		l.Debug(op + " - End")
	}()

	netID, err := hns.CreateNamespace()
	if err != nil {
		return err
	}
	log.G(ctx).WithFields(logrus.Fields{
		"netID":               netID,
		logfields.ContainerID: coi.ID,
	}).Info("created network namespace for container")
	r.SetNetNS(netID)
	r.SetCreatedNetNS(true)
	endpoints := make([]string, 0)
	for _, endpointID := range coi.Spec.Windows.Network.EndpointList {
		err = hns.AddNamespaceEndpoint(netID, endpointID)
		if err != nil {
			return err
		}
		log.G(ctx).WithFields(logrus.Fields{
			"netID":      netID,
			"endpointID": endpointID,
		}).Info("added network endpoint to namespace")
		endpoints = append(endpoints, endpointID)
	}
	r.Add(&uvm.NetworkEndpoints{EndpointIDs: endpoints, Namespace: netID})
	return nil
}

// GetNamespaceEndpoints gets all endpoints in `netNS`
func GetNamespaceEndpoints(ctx context.Context, netNS string) ([]*hns.HNSEndpoint, error) {
	op := "hcsoci::GetNamespaceEndpoints"
	l := log.G(ctx).WithField("netns-id", netNS)
	l.Debug(op + " - Begin")
	defer func() {
		l.Debug(op + " - End")
	}()

	ids, err := hns.GetNamespaceEndpoints(netNS)
	if err != nil {
		return nil, err
	}
	var endpoints []*hns.HNSEndpoint
	for _, id := range ids {
		endpoint, err := hns.GetHNSEndpointByID(id)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints, nil
}

package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"golang.org/x/exp/slices"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	ingressv1alpha1 "github.com/ngrok/ngrok-operator/api/ingress/v1alpha1"
	ngrokv1alpha1 "github.com/ngrok/ngrok-operator/api/ngrok/v1alpha1"

	"github.com/ngrok/ngrok-operator/internal/annotations"
	"github.com/ngrok/ngrok-operator/internal/errors"
	"github.com/ngrok/ngrok-operator/internal/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultClusterDomain = "svc.cluster.local"

const (
	labelControllerNamespace = "k8s.ngrok.com/controller-namespace"
	labelControllerName      = "k8s.ngrok.com/controller-name"
	labelNamespace           = "k8s.ngrok.com/namespace"
	labelServiceUID          = "k8s.ngrok.com/service-uid"
	labelService             = "k8s.ngrok.com/service"
	labelPort                = "k8s.ngrok.com/port"
)

// Driver maintains the store of information, can derive new information from the store, and can
// synchronize the desired state of the store to the actual state of the cluster.
type Driver struct {
	store Storer

	cacheStores          CacheStores
	log                  logr.Logger
	scheme               *runtime.Scheme
	ingressNgrokMetadata string
	gatewayNgrokMetadata string
	managerName          types.NamespacedName
	clusterDomain        string

	syncMu              sync.Mutex
	syncRunning         bool
	syncFullCh          chan error
	syncPartialCh       chan error
	syncAllowConcurrent bool

	gatewayEnabled bool
}

type DriverOpt func(*Driver)

func WithGatewayEnabled(enabled bool) DriverOpt {
	return func(d *Driver) {
		d.gatewayEnabled = enabled
	}
}

func WithSyncAllowConcurrent(allowed bool) DriverOpt {
	return func(d *Driver) {
		d.syncAllowConcurrent = allowed
	}
}

func WithClusterDomain(domain string) DriverOpt {
	return func(d *Driver) {
		d.clusterDomain = domain
	}
}

// NewDriver creates a new driver with a basic logger and cache store setup
func NewDriver(logger logr.Logger, scheme *runtime.Scheme, controllerName string, managerName types.NamespacedName, opts ...DriverOpt) *Driver {
	cacheStores := NewCacheStores(logger)
	s := New(cacheStores, controllerName, logger)
	d := &Driver{
		store:          s,
		cacheStores:    cacheStores,
		log:            logger,
		scheme:         scheme,
		managerName:    managerName,
		gatewayEnabled: false,
		clusterDomain:  defaultClusterDomain,
	}

	for _, opt := range opts {
		opt(d)
	}
	return d
}

// WithNgrokMetadata allows you to pass in custom ngrokmetadata to be added to all resources created by the controller
func (d *Driver) WithNgrokMetadata(customNgrokMetadata map[string]string) *Driver {
	ingressNgrokMetadata, err := d.setNgrokMetadataOwner("kubernetes-ingress-controller", customNgrokMetadata)
	if err != nil {
		d.log.Error(err, "error marshalling custom ngrokmetadata", "customNgrokMetadata", d.ingressNgrokMetadata)
		return d
	}
	d.ingressNgrokMetadata = ingressNgrokMetadata

	if d.gatewayEnabled {
		gatewayNgrokMetadata, err := d.setNgrokMetadataOwner("kubernetes-gateway-api", customNgrokMetadata)
		if err != nil {
			d.log.Error(err, "error marshalling custom ngrokmetadata", "customNgrokMetadata", d.gatewayNgrokMetadata)
			return d
		}
		d.gatewayNgrokMetadata = gatewayNgrokMetadata

	}
	return d
}

func (d *Driver) setNgrokMetadataOwner(owner string, customNgrokMetadata map[string]string) (string, error) {
	metaData := make(map[string]string)
	for k, v := range customNgrokMetadata {
		metaData[k] = v
	}
	if _, ok := metaData["owned-by"]; !ok {
		metaData["owned-by"] = owner
	}
	jsonString, err := json.Marshal(metaData)
	if err != nil {
		return "", err
	}

	return string(jsonString), nil
}

func listObjectsForType(ctx context.Context, client client.Reader, v interface{}) ([]client.Object, error) {
	switch v.(type) {

	// ----------------------------------------------------------------------------
	// Kubernetes Core API Support
	// ----------------------------------------------------------------------------
	case *corev1.Service:
		services := &corev1.ServiceList{}
		err := client.List(ctx, services)
		return util.ToClientObjects(services.Items), err
	case *netv1.Ingress:
		ingresses := &netv1.IngressList{}
		err := client.List(ctx, ingresses)
		return util.ToClientObjects(ingresses.Items), err
	case *netv1.IngressClass:
		ingressClasses := &netv1.IngressClassList{}
		err := client.List(ctx, ingressClasses)
		return util.ToClientObjects(ingressClasses.Items), err

	// ----------------------------------------------------------------------------
	// Kubernetes Gateway API Support
	// ----------------------------------------------------------------------------
	case *gatewayv1.GatewayClass:
		gatewayClasses := &gatewayv1.GatewayClassList{}
		err := client.List(ctx, gatewayClasses)
		return util.ToClientObjects(gatewayClasses.Items), err
	case *gatewayv1.Gateway:
		gateways := &gatewayv1.GatewayList{}
		err := client.List(ctx, gateways)
		return util.ToClientObjects(gateways.Items), err
	case *gatewayv1.HTTPRoute:
		httproutes := &gatewayv1.HTTPRouteList{}
		err := client.List(ctx, httproutes)
		return util.ToClientObjects(httproutes.Items), err

	// ----------------------------------------------------------------------------
	// Ngrok API Support
	// ----------------------------------------------------------------------------
	case *ingressv1alpha1.Domain:
		domains := &ingressv1alpha1.DomainList{}
		err := client.List(ctx, domains)
		return util.ToClientObjects(domains.Items), err
	case *ingressv1alpha1.HTTPSEdge:
		edges := &ingressv1alpha1.HTTPSEdgeList{}
		err := client.List(ctx, edges)
		return util.ToClientObjects(edges.Items), err
	case *ingressv1alpha1.Tunnel:
		tunnels := &ingressv1alpha1.TunnelList{}
		err := client.List(ctx, tunnels)
		return util.ToClientObjects(tunnels.Items), err
	case *ingressv1alpha1.NgrokModuleSet:
		modules := &ingressv1alpha1.NgrokModuleSetList{}
		err := client.List(ctx, modules)
		return util.ToClientObjects(modules.Items), err
	case *ngrokv1alpha1.NgrokTrafficPolicy:
		policies := &ngrokv1alpha1.NgrokTrafficPolicyList{}
		err := client.List(ctx, policies)
		return util.ToClientObjects(policies.Items), err
	}
	return nil, fmt.Errorf("unsupported type %T", v)
}

// Seed fetches all the upfront information the driver needs to operate
// It needs to be seeded fully before it can be used to make calculations otherwise
// each calculation will be based on an incomplete state of the world. It currently relies on:
// - Ingresses
// - IngressClasses
// - Gateways
// - HTTPRoutes
// - Services
// - Domains
// - Edges
// - Tunnels
// - ModuleSets
// - TrafficPolicies
// When the sync method becomes a background process, this likely won't be needed anymore
func (d *Driver) Seed(ctx context.Context, c client.Reader) error {
	typesToSeed := []interface{}{
		&netv1.Ingress{},
		&netv1.IngressClass{},
		&corev1.Service{},
		// CRDs
		&ingressv1alpha1.Domain{},
		&ingressv1alpha1.HTTPSEdge{},
		&ingressv1alpha1.Tunnel{},
		&ingressv1alpha1.NgrokModuleSet{},
		&ngrokv1alpha1.NgrokTrafficPolicy{},
	}

	if d.gatewayEnabled {
		typesToSeed = append(typesToSeed,
			&gatewayv1.Gateway{},
			&gatewayv1.GatewayClass{},
			&gatewayv1.HTTPRoute{},
		)
	}

	for _, v := range typesToSeed {
		objects, err := listObjectsForType(ctx, c, v)
		if err != nil {
			return err
		}

		for _, obj := range objects {
			if err := d.store.Update(obj); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Driver) PrintState(setupLog logr.Logger) {
	ings := d.store.ListNgrokIngressesV1()
	for _, ing := range ings {
		setupLog.Info("found matching ingress", "ingress-name", ing.Name, "ingress-namespace", ing.Namespace)
	}

	// Helpful debug information if someone doesn't have their ingress class set up correctly.
	if len(ings) == 0 {
		ingresses := d.store.ListIngressesV1()
		ngrokIngresses := d.store.ListNgrokIngressesV1()
		ingressClasses := d.store.ListIngressClassesV1()
		ngrokIngressClasses := d.store.ListNgrokIngressClassesV1()
		setupLog.Info("no matching ingresses found",
			"all ingresses", ingresses,
			"all ngrok ingresses", ngrokIngresses,
			"all ingress classes", ingressClasses,
			"all ngrok ingress classes", ngrokIngressClasses,
		)
	}
}

func (d *Driver) UpdateIngress(ingress *netv1.Ingress) (*netv1.Ingress, error) {
	if err := d.store.Update(ingress); err != nil {
		return nil, err
	}
	return d.store.GetNgrokIngressV1(ingress.Name, ingress.Namespace)
}

func (d *Driver) UpdateGateway(gateway *gatewayv1.Gateway) (*gatewayv1.Gateway, error) {
	if err := d.store.Update(gateway); err != nil {
		return nil, err
	}
	return d.store.GetGateway(gateway.Name, gateway.Namespace)
}

func (d *Driver) UpdateHTTPRoute(httproute *gatewayv1.HTTPRoute) (*gatewayv1.HTTPRoute, error) {
	if err := d.store.Update(httproute); err != nil {
		return nil, err
	}
	return d.store.GetHTTPRoute(httproute.Name, httproute.Namespace)
}

func (d *Driver) DeleteIngress(ingress *netv1.Ingress) error {
	return d.store.Delete(ingress)
}

func (d *Driver) DeleteGateway(gateway *gatewayv1.Gateway) error {
	return d.store.Delete(gateway)
}

func (d *Driver) DeleteHTTPRoute(httproute *gatewayv1.HTTPRoute) error {
	return d.store.Delete(httproute)
}

// Delete an ingress object given the NamespacedName
// Takes a namespacedName string as a parameter and
// deletes the ingress object from the cacheStores map
func (d *Driver) DeleteNamedIngress(n types.NamespacedName) error {
	ingress := &netv1.Ingress{}
	// set NamespacedName on the ingress object
	ingress.SetNamespace(n.Namespace)
	ingress.SetName(n.Name)
	return d.cacheStores.Delete(ingress)
}

func (d *Driver) DeleteNamedGateway(n types.NamespacedName) error {
	gtw := &gatewayv1.Gateway{}
	// set NamespacedName on the gateway object
	gtw.SetNamespace(n.Namespace)
	gtw.SetName(n.Name)
	return d.cacheStores.Delete(gtw)
}

func (d *Driver) DeleteNamedHTTPRoute(n types.NamespacedName) error {
	httproute := &gatewayv1.HTTPRoute{}
	// set NamespacedName on the httproute object
	httproute.SetNamespace(n.Namespace)
	httproute.SetName(n.Name)
	return d.cacheStores.Delete(httproute)
}

// syncStart will:
//   - let the first caller proceed, indicated by returning true
//   - while the first one is running any subsequent calls will be batched to the last call
//   - the callers between first and last will be assumed "success" and wait will return nil
//   - the last one will return an error, which will retrigger reconciliation
func (d *Driver) syncStart(partial bool) (bool, func(ctx context.Context) error) {
	d.log.Info("sync start")
	d.syncMu.Lock()
	defer d.syncMu.Unlock()

	if !d.syncRunning {
		// not running, we can take action
		d.syncRunning = true
		return true, nil
	}

	// already running, overtake any other waiters
	if d.syncFullCh != nil {
		if partial {
			// a full sync is already waiting, ignore non-full ones
			return false, func(ctx context.Context) error {
				return nil
			}
		}
		close(d.syncFullCh)
		d.syncFullCh = nil
	}
	if d.syncPartialCh != nil {
		close(d.syncPartialCh)
		d.syncPartialCh = nil
	}

	// put yourself in waiting position
	ch := make(chan error, 1)
	if partial {
		d.syncPartialCh = ch
	} else {
		d.syncFullCh = ch
	}

	return false, func(ctx context.Context) error {
		select {
		case err := <-ch:
			d.log.Info("sync done", "err", err)
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var errSyncDone = errors.New("sync done")

func (d *Driver) syncDone() {
	d.log.Info("sync done")
	d.syncMu.Lock()
	defer d.syncMu.Unlock()

	if d.syncFullCh != nil {
		d.syncFullCh <- errSyncDone
		close(d.syncFullCh)
		d.syncFullCh = nil
	}
	if d.syncPartialCh != nil {
		d.syncPartialCh <- errSyncDone
		close(d.syncPartialCh)
		d.syncPartialCh = nil
	}
	d.syncRunning = false
}

// Sync calculates what the desired state for each of our CRDs should be based on the ingresses and other
// objects in the store. It then compares that to the actual state of the cluster and updates the cluster
func (d *Driver) Sync(ctx context.Context, c client.Client) error {
	// This function gets called a lot in the current architecture. At the end it also syncs
	// resources which in turn triggers more reconcile events. Its all eventually consistent, but
	// its noisy and can make us hit ngrok api limits. We should probably just change this to be
	// a periodic sync instead of a sync on every reconcile event, but for now this debouncer
	// keeps it in check and syncs in batches
	if !d.syncAllowConcurrent {
		if proceed, wait := d.syncStart(false); proceed {
			defer d.syncDone()
		} else {
			return wait(ctx)
		}
	}

	d.log.Info("syncing driver state!!")
	desiredDomains, desiredIngressDomains, desiredGatewayDomainMap := d.calculateDomains()
	desiredEdges := d.calculateHTTPSEdges(&desiredIngressDomains, desiredGatewayDomainMap)
	desiredTunnels := d.calculateTunnels()

	currDomains := &ingressv1alpha1.DomainList{}
	currEdges := &ingressv1alpha1.HTTPSEdgeList{}
	currTunnels := &ingressv1alpha1.TunnelList{}

	if err := c.List(ctx, currDomains); err != nil {
		d.log.Error(err, "error listing domains")
		return err
	}
	if err := c.List(ctx, currEdges, client.MatchingLabels{
		labelControllerNamespace: d.managerName.Namespace,
		labelControllerName:      d.managerName.Name,
	}); err != nil {
		d.log.Error(err, "error listing edges")
		return err
	}
	if err := c.List(ctx, currTunnels, client.MatchingLabels{
		labelControllerNamespace: d.managerName.Namespace,
		labelControllerName:      d.managerName.Name,
	}); err != nil {
		d.log.Error(err, "error listing tunnels")
		return err
	}

	if err := d.applyDomains(ctx, c, desiredDomains, currDomains.Items); err != nil {
		return err
	}

	if err := d.applyHTTPSEdges(ctx, c, desiredEdges, currEdges.Items); err != nil {
		return err
	}

	if err := d.applyTunnels(ctx, c, desiredTunnels, currTunnels.Items); err != nil {
		return err
	}

	if err := d.updateIngressStatuses(ctx, c); err != nil {
		return err
	}

	// UpdateGatewayStatuses
	//if err := d.updateGatewayStatuses(ctx, c); err != nil {
	//	return err
	//}

	// UpdateHTTPRouteStatuses
	//if err := d.updateHTTPRouteStatuses(ctx, c); err != nil {
	//	return err
	//}

	return nil
}

func (d *Driver) SyncEdges(ctx context.Context, c client.Client) error {
	if !d.syncAllowConcurrent {
		if proceed, wait := d.syncStart(true); proceed {
			defer d.syncDone()
		} else {
			return wait(ctx)
		}
	}

	d.log.Info("syncing edges state!!")
	_, desiredIngressDomains, desiredGatewayDomainMap := d.calculateDomains()

	desiredEdges := d.calculateHTTPSEdges(&desiredIngressDomains, desiredGatewayDomainMap)
	currEdges := &ingressv1alpha1.HTTPSEdgeList{}
	if err := c.List(ctx, currEdges, client.MatchingLabels{
		labelControllerNamespace: d.managerName.Namespace,
		labelControllerName:      d.managerName.Name,
	}); err != nil {
		d.log.Error(err, "error listing edges")
		return err
	}

	if err := d.applyHTTPSEdges(ctx, c, desiredEdges, currEdges.Items); err != nil {
		return err
	}

	return nil
}

func (d *Driver) applyDomains(ctx context.Context, c client.Client, desiredDomains, currentDomains []ingressv1alpha1.Domain) error {
	for _, desiredDomain := range desiredDomains {
		found := false
		for _, currDomain := range currentDomains {
			if desiredDomain.Name == currDomain.Name && desiredDomain.Namespace == currDomain.Namespace {
				// It matches so lets update it if anything is different
				if !reflect.DeepEqual(desiredDomain.Spec, currDomain.Spec) {
					currDomain.Spec = desiredDomain.Spec
					if err := c.Update(ctx, &currDomain); err != nil {
						d.log.Error(err, "error updating domain", "domain", desiredDomain)
						return err
					}
				}
				found = true
				break
			}
		}
		if !found {
			if err := c.Create(ctx, &desiredDomain); err != nil {
				d.log.Error(err, "error creating domain", "domain", desiredDomain)
				return err
			}
		}
	}

	// Don't delete domains to prevent accidentally de-registering them and making people re-do DNS

	return nil
}

func (d *Driver) applyHTTPSEdges(ctx context.Context, c client.Client, desiredEdges map[string]ingressv1alpha1.HTTPSEdge, currentEdges []ingressv1alpha1.HTTPSEdge) error {
	// update or delete edge we don't need anymore
	for _, currEdge := range currentEdges {
		hostports := currEdge.Spec.Hostports

		// If one of the controller-owned edges has more than one hostport, log an error and skip it
		// because we can't determine what to do with it.
		if len(hostports) != 1 {
			d.log.Error(nil, "Existing owned edge has more than 1 hostport", "edge", currEdge, "hostports", hostports)
			continue
		}

		// ngrok only supports https on port 443 and all domains are on port 443
		// so we can safely trim the port from the hostport to get the domain
		domain := strings.TrimSuffix(hostports[0], ":443")

		if desiredEdge, ok := desiredEdges[domain]; ok {
			needsUpdate := false

			if !reflect.DeepEqual(desiredEdge.Spec, currEdge.Spec) {
				currEdge.Spec = desiredEdge.Spec
				needsUpdate = true
			}

			if needsUpdate {
				if err := c.Update(ctx, &currEdge); err != nil {
					d.log.Error(err, "error updating edge", "desiredEdge", desiredEdge, "currEdge", currEdge)
					return err
				}
			}

			// matched and updated the edge, no longer desired
			delete(desiredEdges, domain)
		} else {
			if err := c.Delete(ctx, &currEdge); client.IgnoreNotFound(err) != nil {
				d.log.Error(err, "error deleting edge", "edge", currEdge)
				return err
			}
		}
	}

	// the set of desired edges now only contains new edges, create them
	for _, edge := range desiredEdges {
		if err := c.Create(ctx, &edge); err != nil {
			d.log.Error(err, "error creating edge", "edge", edge)
			return err
		}
	}

	return nil
}

func (d *Driver) applyTunnels(ctx context.Context, c client.Client, desiredTunnels map[tunnelKey]ingressv1alpha1.Tunnel, currentTunnels []ingressv1alpha1.Tunnel) error {
	// update or delete tunnels we don't need anymore
	for _, currTunnel := range currentTunnels {
		// extract tunnel key
		tkey := d.tunnelKeyFromTunnel(currTunnel)

		// check if new state still needs this tunnel
		if desiredTunnel, ok := desiredTunnels[tkey]; ok {
			needsUpdate := false

			// compare/update owner references
			if !slices.Equal(desiredTunnel.OwnerReferences, currTunnel.OwnerReferences) {
				needsUpdate = true
				currTunnel.OwnerReferences = desiredTunnel.OwnerReferences
			}

			// compare/update desired tunnel spec
			if !reflect.DeepEqual(desiredTunnel.Spec, currTunnel.Spec) {
				needsUpdate = true
				currTunnel.Spec = desiredTunnel.Spec
			}

			if needsUpdate {
				if err := c.Update(ctx, &currTunnel); err != nil {
					d.log.Error(err, "error updating tunnel", "tunnel", desiredTunnel)
					return err
				}
			}

			// matched and updated the tunnel, no longer desired
			delete(desiredTunnels, tkey)
		} else {
			// no longer needed, delete it
			if err := c.Delete(ctx, &currTunnel); client.IgnoreNotFound(err) != nil {
				d.log.Error(err, "error deleting tunnel", "tunnel", currTunnel)
				return err
			}
		}
	}

	// the set of desired tunnels now only contains new tunnels, create them
	for _, tunnel := range desiredTunnels {
		if err := c.Create(ctx, &tunnel); err != nil {
			d.log.Error(err, "error creating tunnel", "tunnel", tunnel)
			return err
		}
	}

	return nil
}

func (d *Driver) updateIngressStatuses(ctx context.Context, c client.Client) error {
	ingresses := d.store.ListNgrokIngressesV1()
	for _, ingress := range ingresses {
		newLBIPStatus := d.calculateIngressLoadBalancerIPStatus(ingress, c)
		if !reflect.DeepEqual(ingress.Status.LoadBalancer.Ingress, newLBIPStatus) {
			ingress.Status.LoadBalancer.Ingress = newLBIPStatus
			if err := c.Status().Update(ctx, ingress); err != nil {
				d.log.Error(err, "error updating ingress status", "ingress", ingress)
				return err
			}
		}
	}
	return nil
}

func (d *Driver) calculateDomains() ([]ingressv1alpha1.Domain, []ingressv1alpha1.Domain, map[string]ingressv1alpha1.Domain) {
	var domains, ingressDomains []ingressv1alpha1.Domain
	ingressDomainMap := d.calculateDomainsFromIngress()

	ingressDomains = make([]ingressv1alpha1.Domain, 0, len(ingressDomainMap))
	for _, domain := range ingressDomainMap {
		ingressDomains = append(ingressDomains, domain)
		domains = append(domains, domain)
	}

	var gatewayDomainMap map[string]ingressv1alpha1.Domain
	if d.gatewayEnabled {
		gatewayDomainMap = d.calculateDomainsFromGateway(ingressDomainMap)
		for _, domain := range gatewayDomainMap {
			domains = append(domains, domain)
		}
	}

	return domains, ingressDomains, gatewayDomainMap
}

func (d *Driver) calculateDomainsFromIngress() map[string]ingressv1alpha1.Domain {
	domainMap := make(map[string]ingressv1alpha1.Domain)

	ingresses := d.store.ListNgrokIngressesV1()
	for _, ingress := range ingresses {
		for _, rule := range ingress.Spec.Rules {
			if rule.Host == "" {
				continue
			}

			domain := ingressv1alpha1.Domain{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ingressv1alpha1.HyphenatedDomainNameFromURL(rule.Host),
					Namespace: ingress.Namespace,
				},
				Spec: ingressv1alpha1.DomainSpec{
					Domain: rule.Host,
				},
			}
			domain.Spec.Metadata = d.ingressNgrokMetadata
			domainMap[rule.Host] = domain
		}
	}

	return domainMap
}

func (d *Driver) calculateDomainsFromGateway(ingressDomains map[string]ingressv1alpha1.Domain) map[string]ingressv1alpha1.Domain {
	domainMap := make(map[string]ingressv1alpha1.Domain)

	gateways := d.store.ListGateways()
	for _, gw := range gateways {
		for _, listener := range gw.Spec.Listeners {
			if listener.Hostname == nil {
				continue
			}
			domainName := string(*listener.Hostname)
			if _, hasVal := ingressDomains[domainName]; hasVal {
				// TODO update gateway status
				// also add error to error page
				continue
			}
			domain := ingressv1alpha1.Domain{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ingressv1alpha1.HyphenatedDomainNameFromURL(domainName),
					Namespace: gw.Namespace,
				},
				Spec: ingressv1alpha1.DomainSpec{
					Domain: domainName,
				},
			}
			domain.Spec.Metadata = d.gatewayNgrokMetadata
			domainMap[domainName] = domain
		}
	}

	return domainMap
}

// Given an ingress, it will resolve any ngrok modulesets defined on the ingress to the
// CRDs and then will merge them in to a single moduleset
func (d *Driver) getNgrokModuleSetForIngress(ing *netv1.Ingress) (*ingressv1alpha1.NgrokModuleSet, error) {
	computedModSet := &ingressv1alpha1.NgrokModuleSet{}

	modules, err := annotations.ExtractNgrokModuleSetsFromAnnotations(ing)
	if err != nil {
		if errors.IsMissingAnnotations(err) {
			return computedModSet, nil
		}
		return computedModSet, err
	}

	for _, module := range modules {
		resolvedMod, err := d.store.GetNgrokModuleSetV1(module, ing.Namespace)
		if err != nil {
			return computedModSet, err
		}
		computedModSet.Merge(resolvedMod)
	}

	return computedModSet, nil
}

func (d *Driver) getNgrokTrafficPolicyForIngress(ing *netv1.Ingress) (*ngrokv1alpha1.NgrokTrafficPolicy, error) {
	policy, err := annotations.ExtractNgrokTrafficPolicyFromAnnotations(ing)
	if err != nil {
		if errors.IsMissingAnnotations(err) {
			return nil, nil
		}
		return nil, err
	}

	return d.store.GetNgrokTrafficPolicyV1(policy, ing.Namespace)
}

func (d *Driver) calculateHTTPSEdges(ingressDomains *[]ingressv1alpha1.Domain, gatewayDomainMap map[string]ingressv1alpha1.Domain) map[string]ingressv1alpha1.HTTPSEdge {
	edgeMap := make(map[string]ingressv1alpha1.HTTPSEdge, len(*ingressDomains))
	for _, domain := range *ingressDomains {
		edge := ingressv1alpha1.HTTPSEdge{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: domain.Name + "-",
				Namespace:    domain.Namespace,
				Labels:       d.edgeLabels(),
			},
			Spec: ingressv1alpha1.HTTPSEdgeSpec{
				Hostports: []string{domain.Spec.Domain + ":443"},
			},
		}
		edge.Spec.Metadata = d.ingressNgrokMetadata
		edgeMap[domain.Spec.Domain] = edge
	}
	d.calculateHTTPSEdgesFromIngress(edgeMap)

	if d.gatewayEnabled {
		gatewayEdgeMap := make(map[string]ingressv1alpha1.HTTPSEdge)
		httproutes := d.store.ListHTTPRoutes()
		gateways := d.store.ListGateways()
		for _, gtw := range gateways {
			gatewayDomains := make(map[string]string)
			for _, listener := range gtw.Spec.Listeners {
				if listener.Hostname == nil {
					continue
				}
				if listener.Protocol != gatewayv1.HTTPSProtocolType || int(listener.Port) != 443 {
					continue
				}
				if _, hasDomain := gatewayDomainMap[string(*listener.Hostname)]; !hasDomain {
					continue
				}
				gatewayDomains[string(*listener.Hostname)] = string(*listener.Hostname)
			}
			if len(gatewayDomains) == 0 {
				d.log.Info("no usable domains in gateway, may be missing https listener", "gateway", gtw.Name)
				continue
			}
			for _, httproute := range httproutes {
				var routeDomains []string
				for _, parent := range httproute.Spec.ParentRefs {
					if string(parent.Name) != gtw.Name {
						continue
					}
					var domainOverlap []string
					for _, hostname := range httproute.Spec.Hostnames {
						domain := string(hostname)
						if _, hasDomain := gatewayDomains[domain]; hasDomain {
							domainOverlap = append(domainOverlap, domain)
						}
					}
					if len(domainOverlap) == 0 {
						// no hostnames overlap with gateway
						continue
					}
					routeDomains = append(routeDomains, domainOverlap...)
				}
				if len(routeDomains) == 0 {
					// no usable domains in route
					continue
				}
				var hostPorts []string

				for _, domain := range routeDomains {
					hostPorts = append(hostPorts, domain+":443")
				}
				edge := ingressv1alpha1.HTTPSEdge{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: httproute.Name + "-",
						Namespace:    httproute.Namespace,
						Labels:       d.edgeLabels(),
					},
					Spec: ingressv1alpha1.HTTPSEdgeSpec{
						Hostports: hostPorts,
					},
				}
				edge.Spec.Metadata = d.gatewayNgrokMetadata
				gatewayEdgeMap[routeDomains[0]] = edge

			}
		}
		d.calculateHTTPSEdgesFromGateway(gatewayEdgeMap)

		// merge edge maps
		for k, v := range gatewayEdgeMap {
			edgeMap[k] = v
		}
	}

	return edgeMap
}

func (d *Driver) calculateHTTPSEdgesFromIngress(edgeMap map[string]ingressv1alpha1.HTTPSEdge) {
	ingresses := d.store.ListNgrokIngressesV1()
	for _, ingress := range ingresses {
		modSet, err := d.getNgrokModuleSetForIngress(ingress)
		if err != nil {
			d.log.Error(err, "error getting ngrok moduleset for ingress", "ingress", ingress)
			continue
		}

		policyJSON, err := d.getTrafficPolicyJSON(ingress, modSet)
		if err != nil {
			d.log.Error(err, "error marshalling JSON Policy for ingress", "ingress", ingress)
			continue
		}

		for _, rule := range ingress.Spec.Rules {
			edge, ok := edgeMap[rule.Host]
			if !ok {
				d.log.Error(err, "could not find edge associated with rule", "host", rule.Host)
				continue
			}

			if modSet.Modules.TLSTermination != nil && modSet.Modules.TLSTermination.MinVersion != nil {
				edge.Spec.TLSTermination = &ingressv1alpha1.EndpointTLSTerminationAtEdge{
					MinVersion: ptr.Deref(modSet.Modules.TLSTermination.MinVersion, ""),
				}
			}

			if modSet.Modules.MutualTLS != nil {
				edge.Spec.MutualTLS = modSet.Modules.MutualTLS
			}

			// If any rule for an ingress matches, then it applies to this ingress
			for _, httpIngressPath := range rule.HTTP.Paths {
				matchType := "path_prefix"
				if httpIngressPath.PathType != nil {
					switch *httpIngressPath.PathType {
					case netv1.PathTypePrefix:
						matchType = "path_prefix"
					case netv1.PathTypeExact:
						matchType = "exact_path"
					case netv1.PathTypeImplementationSpecific:
						matchType = "path_prefix" // Path Prefix seems like a sane default for most cases
					default:
						d.log.Error(fmt.Errorf("unknown path type"), "unknown path type", "pathType", *httpIngressPath.PathType)
						continue
					}
				}

				// We only support service backends right now. TODO: support resource backends
				if httpIngressPath.Backend.Service == nil {
					continue
				}

				serviceName := httpIngressPath.Backend.Service.Name
				serviceUID, servicePort, err := d.getEdgeBackend(*httpIngressPath.Backend.Service, ingress.Namespace)
				if err != nil {
					d.log.Error(err, "could not find port for service", "namespace", ingress.Namespace, "service", serviceName)
					continue
				}

				route := ingressv1alpha1.HTTPSEdgeRouteSpec{
					Match:     httpIngressPath.Path,
					MatchType: matchType,
					Backend: ingressv1alpha1.TunnelGroupBackend{
						Labels: d.ngrokLabels(ingress.Namespace, serviceUID, serviceName, servicePort),
					},
					CircuitBreaker:      modSet.Modules.CircuitBreaker,
					Compression:         modSet.Modules.Compression,
					IPRestriction:       modSet.Modules.IPRestriction,
					Headers:             modSet.Modules.Headers,
					OAuth:               modSet.Modules.OAuth,
					Policy:              policyJSON,
					OIDC:                modSet.Modules.OIDC,
					SAML:                modSet.Modules.SAML,
					WebhookVerification: modSet.Modules.WebhookVerification,
				}
				route.Metadata = d.ingressNgrokMetadata

				// Loop through existing routes and check if any match the path and match type
				// If they do, warn about it and continue replacing it
				for _, existingRoute := range edge.Spec.Routes {
					if existingRoute.Match == route.Match && existingRoute.MatchType == route.MatchType {
						d.log.Info("replacing existing route", "route", existingRoute.Match, "newRoute", route.Match)
						continue
					}
				}

				edge.Spec.Routes = append(edge.Spec.Routes, route)
			}

			edgeMap[rule.Host] = edge
		}
	}
}

// getTrafficPolicyJSON retrieves the traffic policy for an ingress and falls back to the modSet policy if it doesn't exist.
func (d *Driver) getTrafficPolicyJSON(ingress *netv1.Ingress, modSet *ingressv1alpha1.NgrokModuleSet) (json.RawMessage, error) {
	var err error
	var policyJSON json.RawMessage

	trafficPolicy, err := d.getNgrokTrafficPolicyForIngress(ingress)

	if err != nil {
		d.log.Error(err, "error getting ngrok traffic policy for ingress", "ingress", ingress)
		return nil, err
	}

	if modSet.Modules.Policy != nil && trafficPolicy != nil {
		return nil, fmt.Errorf("cannot have both a traffic policy and a moduleset policy on ingress: %s", ingress.Name)
	}

	if trafficPolicy != nil {
		return trafficPolicy.Spec.Policy, nil
	}

	if modSet == nil {
		return policyJSON, nil
	}

	if policyJSON, err = json.Marshal(modSet.Modules.Policy); err != nil {
		d.log.Error(err, "cannot convert module-set policy json", "ingress", ingress, "Policy", modSet.Modules.Policy)
		return nil, err
	}

	return policyJSON, nil
}

func (d *Driver) calculateHTTPSEdgesFromGateway(edgeMap map[string]ingressv1alpha1.HTTPSEdge) {
	gateways := d.store.ListGateways()

	for _, gtw := range gateways {
		for _, listener := range gtw.Spec.Listeners {
			if listener.Hostname == nil {
				continue
			}
			allowedRoutes := listener.AllowedRoutes.Kinds
			if len(allowedRoutes) > 0 {
				createHttpsedge := false
				for _, routeKind := range allowedRoutes {
					if routeKind.Kind == "HTTPRoute" {
						createHttpsedge = true
					}
				}
				if !createHttpsedge {
					continue
				}
			}
			domainName := string(*listener.Hostname)
			edge, ok := edgeMap[domainName]
			if !ok {
				continue
			}
			// TODO: Calculate routes from httpRoutes
			// TODO: skip if no backend services
			httproutes := d.store.ListHTTPRoutes()
			for _, httproute := range httproutes {
				for _, parent := range httproute.Spec.ParentRefs {
					if string(parent.Name) != gtw.Name {
						// not our gateway so skip
						continue
					}

					if listener.AllowedRoutes != nil && listener.AllowedRoutes.Namespaces.From != nil {
						switch *listener.AllowedRoutes.Namespaces.From {
						case gatewayv1.NamespacesFromAll:
						case gatewayv1.NamespacesFromSame:
							if httproute.Namespace != gtw.Namespace {
								continue
							}
						case gatewayv1.NamespacesFromSelector:
							if httproute.Namespace != listener.AllowedRoutes.Namespaces.Selector.String() {
								continue
							}
						}
					}

					// matches our gateway
					for _, hostname := range httproute.Spec.Hostnames {
						if string(hostname) != string(*listener.Hostname) {
							// doesn't match this listener
							continue
						}
						// matches gateway and listener
						for _, rule := range httproute.Spec.Rules {
							// TODO: resolve rule.Matches
							// TODO: resolve rule.Filters
							// for v0 we will only resolve the first backendRef
							pathMatch := "/"
							pathMatchType := "path_prefix"
							// first match with a path will be accepted as the route's path
							for _, match := range rule.Matches {
								if match.Path != nil {
									pathMatch = *match.Path.Value
									if *match.Path.Type == gatewayv1.PathMatchExact {
										pathMatchType = "exact_path"
									}
									break
								}
							}
							route := ingressv1alpha1.HTTPSEdgeRouteSpec{
								Match:     pathMatch,     // change based on the rule.match
								MatchType: pathMatchType, // change based on rule.Matches
							}

							// TODO: set with values from rules.Filters + rules.Matches
							// this HTTPRouteRule comes direct from gateway api yaml, and func returns the policy,
							// which goes directly into the edge route in ngrok.
							policy, err := d.createEndpointPolicyForGateway(&rule, httproute.Namespace)
							if err != nil {
								d.log.Error(err, "error creating policy from HTTPRouteRule", "rule", rule)
								continue
							}

							route.Policy = policy

							for idx, backendref := range rule.BackendRefs {
								// currently the ingress controller doesn't support weighted backends
								// so we'll only support one backendref per rule
								// TODO: remove when tested with multiple backends
								if idx > 0 {
									break
								}
								// handle backendref
								refKind := string(*backendref.Kind)
								if refKind != "Service" {
									// only support services currently
									continue
								}

								refName := string(backendref.Name)
								serviceUID, servicePort, err := d.getEdgeBackendRef(backendref.BackendRef, httproute.Namespace)
								if err != nil {
									d.log.Error(err, "could not find port for service", "namespace", httproute.Namespace, "service", refName)
									continue
								}

								route.Backend = ingressv1alpha1.TunnelGroupBackend{
									Labels: d.ngrokLabels(httproute.Namespace, serviceUID, refName, servicePort),
								}

							}
							route.Metadata = d.gatewayNgrokMetadata

							edge.Spec.Routes = append(edge.Spec.Routes, route)
						}
					}
				}
			}

			edgeMap[domainName] = edge
		}
	}
}

func (d *Driver) createEndpointPolicyForGateway(rule *gatewayv1.HTTPRouteRule, namespace string) (json.RawMessage, error) {
	pathPrefixMatches := []string{}

	// NOTE: matches are only defined on requests, and fitlers are only triggered by matches,
	// but some fitlers define transformations on responses, so we need to define matches on both
	// Policy.Inbound and Policy.Outbound when possible to work with ngrok's system
	for _, match := range rule.Matches {
		if match.Path != nil {
			if match.Path.Type != nil {
				switch *match.Path.Type {
				case gatewayv1.PathMatchExact:
				case gatewayv1.PathMatchPathPrefix:
					if match.Path.Value != nil {
						pathPrefixMatches = append(pathPrefixMatches, *match.Path.Value)
					}
				case gatewayv1.PathMatchRegularExpression:
					return nil, errors.NewErrorNotFound(fmt.Sprintf("unsupported match type PathMatchType %v found", *match.Path.Type))
				default:
					return nil, errors.NewErrorNotFound(fmt.Sprintf("Unknown match type PathMatchType %v found", *match.Path.Type))
				}
			}
		}

		if match.Method != nil {
			d.log.Error(fmt.Errorf("unsupported match type"), "Unsupported match type", "HTTPMethod", *match.Method)
		}

		if len(match.Headers) > 0 {
			d.log.Error(fmt.Errorf("unsupported match type"), "Unsupported match type", "HTTPHeaderMatch", match.Headers)
		}

		if len(match.QueryParams) > 0 {
			d.log.Error(fmt.Errorf("unsupported match type"), "Unsupported match type", "HTTPQueryParamMatch", match.QueryParams)
		}
	}

	fullTrafficPolicy := util.NewTrafficPolicy()

	// "hard-coded" phases. Since Filters are translated to rules in particular phases, the operator has to be aware of these.
	// There isn't really a way around this.
	onHttpRequestActions := util.Actions{}
	onHttpResponseActions := util.Actions{}

	flushCount := 0

	flushActionsToRules := func() error {
		if len(onHttpRequestActions.EndpointActions) == 0 && len(onHttpResponseActions.EndpointActions) == 0 {
			return nil
		}
		// there are actions to flush
		flushCount++
		if len(onHttpRequestActions.EndpointActions) > 0 {
			// flush actions to a rule
			rule := util.EndpointRule{
				Actions: onHttpRequestActions.EndpointActions,
				Name:    fmt.Sprint("Inbound HTTPRouteRule ", flushCount),
			}
			if err := fullTrafficPolicy.MergeEndpointRule(rule, util.PhaseOnHttpRequest); err != nil {
				return err
			}

			// clear
			onHttpRequestActions = util.Actions{}
		}
		if len(onHttpResponseActions.EndpointActions) > 0 {
			// flush actions to a rule
			rule := util.EndpointRule{
				Actions: onHttpResponseActions.EndpointActions,
				Name:    fmt.Sprint("Outbound HTTPRouteRule ", flushCount),
			}
			if err := fullTrafficPolicy.MergeEndpointRule(rule, util.PhaseOnHttpResponse); err != nil {
				return err
			}

			// clear
			onHttpResponseActions = util.Actions{}
		}

		return nil
	}

	responseHeaders := make(map[string]string)
	for _, filter := range rule.Filters {
		switch filter.Type {
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			// NOTE: request redirect is a special case, and is subject to change
			err := d.handleRequestRedirectFilter(filter.RequestRedirect, pathPrefixMatches, &onHttpRequestActions, responseHeaders)
			if err != nil {
				return nil, err
			}
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			err := d.handleHTTPHeaderFilter(filter.RequestHeaderModifier, &onHttpRequestActions, nil)
			if err != nil {
				return nil, err
			}
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			err := d.handleHTTPHeaderFilter(filter.ResponseHeaderModifier, &onHttpResponseActions, responseHeaders)
			if err != nil {
				return nil, err
			}
		case gatewayv1.HTTPRouteFilterURLRewrite:
			err := d.handleURLRewriteFilter(filter.URLRewrite, pathPrefixMatches, &onHttpRequestActions)
			if err != nil {
				return nil, err
			}
		case gatewayv1.HTTPRouteFilterRequestMirror:
			return nil, errors.NewErrorNotFound(fmt.Sprintf("Unsupported filter HTTPRouteFilterType %v found", filter.Type))
		case gatewayv1.HTTPRouteFilterExtensionRef:
			// if there are current actions outstanding, make a rule to hold them before we start a new rule for this PolicyCRD
			if err := flushActionsToRules(); err != nil {
				return nil, err
			}

			// a PolicyCRD can have expressions, so send in rule pointers so expressions can be on those rules
			err := d.handleExtensionRef(filter.ExtensionRef, namespace, fullTrafficPolicy)
			if err != nil {
				return nil, err
			}
		default:
			return nil, errors.NewErrorNotFound(fmt.Sprintf("Unknown filter HTTPRouteFilterType %v found", filter.Type))
		}
	}

	// flush any leftover actions to rules
	if err := flushActionsToRules(); err != nil {
		return nil, err
	}

	policy, err := fullTrafficPolicy.ToCRDJson()
	if err != nil {
		return nil, err
	}

	return policy, nil
}

type RemoveHeadersConfig struct {
	Headers []string `json:"headers"`
}

type AddHeadersConfig struct {
	Headers map[string]string `json:"headers"`
}

// extractPolicy parses the policy message into a format such that it can be combined with policy from other filters.
// If the legacy "inbound/outbound" format is detected, inbound remaps to `on_http_request`, outbound remaps to
// `on_http_response`. This is safe so long as HTTP Edges are the only ones supported on the gateway API.
func extractPolicy(jsonMessage json.RawMessage) (util.TrafficPolicy, error) {
	extensionRefTrafficPolicy, err := util.NewTrafficPolicyFromJson(jsonMessage)
	if err != nil {
		return nil, err
	}

	if extensionRefTrafficPolicy.IsLegacyPolicy() {
		extensionRefTrafficPolicy.ConvertLegacyDirectionsToPhases()
	}

	return extensionRefTrafficPolicy, nil
}

func (d *Driver) handleExtensionRef(extensionRef *gatewayv1.LocalObjectReference, namespace string, trafficPolicy util.TrafficPolicy) error {
	switch extensionRef.Kind {
	case "NgrokTrafficPolicy":
		// look up Policy CRD
		policy, err := d.store.GetNgrokTrafficPolicyV1(string(extensionRef.Name), namespace)
		if err != nil {
			return err
		}

		jsonMessage := policy.Spec.Policy
		if jsonMessage == nil {
			return errors.NewErrorNotFound(fmt.Sprintf("PolicyCRD %v found with no policy", extensionRef.Name))
		}

		// transform into structured format
		extensionRefTrafficPolicy, err := extractPolicy(jsonMessage)
		if err != nil {
			return err
		}

		trafficPolicy.Merge(extensionRefTrafficPolicy)
	default:
		return errors.NewErrorNotFound(fmt.Sprintf("Unknown ExtensionRef Kind %v found, Name: %v", extensionRef.Kind, extensionRef.Name))
	}
	return nil
}

func (d *Driver) handleHTTPHeaderFilter(filter *gatewayv1.HTTPHeaderFilter, actions *util.Actions, requestRedirectHeaders map[string]string) error {
	if filter == nil {
		return nil
	}

	if err := d.handleHTTPHeaderFilterRemove(filter.Remove, actions); err != nil {
		return err
	}

	if err := d.handleHTTPHeaderFilterAdd(filter.Add, actions, requestRedirectHeaders); err != nil {
		return err
	}

	if err := d.handleHTTPHeaderFilterSet(filter, actions, requestRedirectHeaders); err != nil {
		return err
	}

	return nil
}

func (d *Driver) handleHTTPHeaderFilterRemove(headersToRemove []string, actions *util.Actions) error {
	if len(headersToRemove) == 0 {
		return nil
	}

	removeHeaders, err := json.Marshal(RemoveHeadersConfig{Headers: headersToRemove})
	if err != nil {
		d.log.Error(err, "cannot convert headers to json", "headers", headersToRemove)
		return err
	}

	action := util.EndpointAction{
		Type:   "remove-headers",
		Config: removeHeaders,
	}

	rawAction, err := json.Marshal(&action)
	if err != nil {
		return err
	}

	actions.EndpointActions = append(actions.EndpointActions, rawAction)

	return nil
}

func (d *Driver) handleHTTPHeaderFilterAdd(headersToAdd []gatewayv1.HTTPHeader, actions *util.Actions, requestRedirectHeaders map[string]string) error {
	if len(headersToAdd) == 0 {
		return nil
	}

	config := AddHeadersConfig{Headers: make(map[string]string)}
	for _, header := range headersToAdd {
		config.Headers[string(header.Name)] = header.Value
	}

	if requestRedirectHeaders != nil {
		for k, v := range config.Headers {
			requestRedirectHeaders[k] = v
		}
	}

	addHeaders, err := json.Marshal(config)
	if err != nil {
		d.log.Error(err, "cannot convert headers to json", "headers", headersToAdd)
		return err
	}

	action := util.EndpointAction{
		Type:   "add-headers",
		Config: addHeaders,
	}

	rawAction, err := json.Marshal(&action)
	if err != nil {
		return nil
	}

	actions.EndpointActions = append(actions.EndpointActions, rawAction)

	return nil
}

func (d *Driver) handleHTTPHeaderFilterSet(filter *gatewayv1.HTTPHeaderFilter, actions *util.Actions, requestRedirectHeaders map[string]string) error {
	if filter == nil {
		return nil
	}
	removeHeaders := []string{}
	for _, header := range filter.Set {
		removeHeaders = append(removeHeaders, string(header.Name))
	}

	if err := d.handleHTTPHeaderFilterRemove(removeHeaders, actions); err != nil {
		return err
	}

	if err := d.handleHTTPHeaderFilterAdd(filter.Set, actions, requestRedirectHeaders); err != nil {
		return err
	}

	return nil
}

type URLRedirectConfig struct {
	To         *string `json:"to"`
	From       *string `json:"from"`
	StatusCode *int    `json:"status_code"`
	// convert to response headers
	Headers map[string]string `json:"headers"`
}

func (d *Driver) createUrlRedirectConfig(from string, to string, requestHeaders map[string]string, statusCode *int, actions *util.Actions) error {
	urlRedirectAction := URLRedirectConfig{
		To:         &to,
		From:       &from,
		StatusCode: statusCode,
		Headers:    requestHeaders,
	}
	config, err := json.Marshal(urlRedirectAction)

	if err != nil {
		d.log.Error(err, "cannot convert request redirect filter to json", "HTTPRequestRedirectFilter", urlRedirectAction)
		return err
	}

	action := util.EndpointAction{
		Type:   "redirect",
		Config: config,
	}

	rawAction, err := json.Marshal(&action)
	if err != nil {
		return err
	}

	actions.EndpointActions = append(actions.EndpointActions, rawAction)

	return nil
}

type URLRewriteConfig struct {
	To   *string `json:"to"`
	From *string `json:"from"`
}

func (d *Driver) createURLRewriteConfig(from string, to string, actions *util.Actions) error {
	urlRewriteAction := URLRewriteConfig{
		To:   &to,
		From: &from,
	}
	config, err := json.Marshal(urlRewriteAction)

	if err != nil {
		d.log.Error(err, "cannot convert request rewrite filter to json", "HTTPRequestRewriteFilter", urlRewriteAction)
		return err
	}

	action := util.EndpointAction{
		Type:   "url-rewrite",
		Config: config,
	}

	rawAction, err := json.Marshal(&action)
	if err != nil {
		return err
	}

	actions.EndpointActions = append(actions.EndpointActions, rawAction)

	return nil
}

func (d *Driver) handleURLRewriteFilter(filter *gatewayv1.HTTPURLRewriteFilter, pathPrefixMatches []string, actions *util.Actions) error {
	var err error
	if filter == nil {
		return nil
	}

	if filter.Hostname != nil {
		hostname := string(*filter.Hostname)
		err = d.handleHTTPHeaderFilterAdd([]gatewayv1.HTTPHeader{{Name: "Host", Value: hostname}}, actions, nil)
	}

	if err != nil {
		return err
	}

	if filter.Path == nil {
		return nil
	}

	switch filter.Path.Type {
	case "ReplacePrefixMatch":
		for _, pathPrefix := range pathPrefixMatches {
			from := fmt.Sprintf("^https?://[^/:]+(:[0-9]*)?(%s)([^\\?]*)(\\?.*)?$", pathPrefix)
			to := fmt.Sprintf("$scheme://$authority%s$3$is_args$args", *filter.Path.ReplacePrefixMatch)
			err := d.createURLRewriteConfig(from, to, actions)
			if err != nil {
				return err
			}
		}
	case "ReplaceFullPath":
		from := ".*" //"^https?://[^/]+(:[0-9]*)?(/[^\\?]*)?(\\?.*)?$"
		to := fmt.Sprintf("$scheme://$authority%s$is_args$args", *filter.Path.ReplaceFullPath)
		err := d.createURLRewriteConfig(from, to, actions)
		if err != nil {
			return err
		}
	default:
		d.log.Error(fmt.Errorf("Unsupported path modifier type"), "unsupported path modifier type", "HTTPPathModifier", filter.Path.Type)
		return nil
	}
	return nil
}

func (d *Driver) handleRequestRedirectFilter(filter *gatewayv1.HTTPRequestRedirectFilter, pathPrefixMatches []string, actions *util.Actions, requestHeaders map[string]string) error {
	if filter == nil {
		return nil
	}

	scheme := "$scheme"
	if filter.Scheme != nil {
		scheme = *filter.Scheme
	}
	hostname := "$host"
	if filter.Hostname != nil {
		hostname = string(*filter.Hostname)
	}
	port := "$1" // (:[0-9]*)?
	if filter.Port != nil {
		port = string(*filter.Port)
	}

	if filter.Path == nil {
		from := ".*" //"^https?://[^/]+(:[0-9]*)?(/[^\\?]*)?(\\?.*)?$"
		to := fmt.Sprintf("%s://%s%s$uri", scheme, hostname, port)
		err := d.createUrlRedirectConfig(from, to, requestHeaders, filter.StatusCode, actions)
		if err != nil {
			return err
		}
		return nil
	}

	switch filter.Path.Type {
	case "ReplacePrefixMatch":
		for _, pathPrefix := range pathPrefixMatches {
			from := fmt.Sprintf("^https?://[^/:]+(:[0-9]*)?(%s)([^\\?]*)(\\?.*)?$", pathPrefix)
			to := fmt.Sprintf("%s://%s%s%s$3$is_args$args", scheme, hostname, port, *filter.Path.ReplacePrefixMatch)
			err := d.createUrlRedirectConfig(from, to, requestHeaders, filter.StatusCode, actions)
			if err != nil {
				return err
			}
		}
	case "ReplaceFullPath":
		from := ".*" //"^https?://[^/]+(:[0-9]*)?(/[^\\?]*)?(\\?.*)?$"
		to := fmt.Sprintf("%s://%s%s%s$is_args$args", scheme, hostname, port, *filter.Path.ReplaceFullPath)
		err := d.createUrlRedirectConfig(from, to, requestHeaders, filter.StatusCode, actions)
		if err != nil {
			return err
		}
	default:
		d.log.Error(fmt.Errorf("unsupported path modifier type"), "unsupported path modifier type", "HTTPPathModifier", filter.Path.Type)
		return nil
	}
	return nil
}

type tunnelKey struct {
	namespace string
	service   string
	port      string
}

func (d *Driver) tunnelKeyFromTunnel(tunnel ingressv1alpha1.Tunnel) tunnelKey {
	return tunnelKey{
		namespace: tunnel.Namespace,
		service:   tunnel.Labels[labelService],
		port:      tunnel.Labels[labelPort],
	}
}

func (d *Driver) calculateTunnels() map[tunnelKey]ingressv1alpha1.Tunnel {
	tunnels := map[tunnelKey]ingressv1alpha1.Tunnel{}
	d.calculateTunnelsFromIngress(tunnels)
	d.calculateTunnelsFromGateway(tunnels)
	return tunnels
}

func (d *Driver) calculateTunnelsFromIngress(tunnels map[tunnelKey]ingressv1alpha1.Tunnel) {
	for _, ingress := range d.store.ListNgrokIngressesV1() {
		for _, rule := range ingress.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				// We only support service backends right now.
				// TODO: support resource backends
				if path.Backend.Service == nil {
					continue
				}

				serviceName := path.Backend.Service.Name
				serviceUID, servicePort, protocol, appProtocol, err := d.getTunnelBackend(*path.Backend.Service, ingress.Namespace)
				if err != nil {
					d.log.Error(err, "could not find port for service", "namespace", ingress.Namespace, "service", serviceName)
				}

				key := tunnelKey{ingress.Namespace, serviceName, strconv.Itoa(int(servicePort))}
				tunnel, found := tunnels[key]
				if !found {
					targetAddr := fmt.Sprintf("%s.%s.%s:%d", serviceName, key.namespace, d.clusterDomain, servicePort)
					tunnel = ingressv1alpha1.Tunnel{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName:    fmt.Sprintf("%s-%d-", serviceName, servicePort),
							Namespace:       ingress.Namespace,
							OwnerReferences: nil, // fill owner references below
							Labels:          d.tunnelLabels(serviceName, servicePort),
						},
						Spec: ingressv1alpha1.TunnelSpec{
							ForwardsTo: targetAddr,
							Labels:     d.ngrokLabels(ingress.Namespace, serviceUID, serviceName, servicePort),
							BackendConfig: &ingressv1alpha1.BackendConfig{
								Protocol: protocol,
							},
							AppProtocol: appProtocol,
						},
					}
				}

				hasIngressReference := false
				for _, ref := range tunnel.OwnerReferences {
					if ref.UID == ingress.UID {
						hasIngressReference = true
						break
					}
				}
				if !hasIngressReference {
					tunnel.OwnerReferences = append(tunnel.OwnerReferences, metav1.OwnerReference{
						APIVersion: ingress.APIVersion,
						Kind:       ingress.Kind,
						Name:       ingress.Name,
						UID:        ingress.UID,
					})
					slices.SortStableFunc(tunnel.OwnerReferences, func(i, j metav1.OwnerReference) int {
						return cmp.Compare(string(i.UID), string(j.UID))
					})
				}

				tunnels[key] = tunnel
			}
		}
	}
}

func (d *Driver) calculateTunnelsFromGateway(tunnels map[tunnelKey]ingressv1alpha1.Tunnel) {
	httproutes := d.store.ListHTTPRoutes()

	for _, httproute := range httproutes {
		for _, rule := range httproute.Spec.Rules {
			for _, backendRef := range rule.BackendRefs {
				// We only support service backends right now.
				// TODO: support resource backends

				//if path.Backend.Service == nil {
				//	continue
				//}

				serviceName := string(backendRef.Name)
				serviceUID, servicePort, protocol, appProtocol, err := d.getTunnelBackendFromGateway(backendRef.BackendRef, httproute.Namespace)
				if err != nil {
					d.log.Error(err, "could not find port for service", "namespace", httproute.Namespace, "service", serviceName)
				}

				key := tunnelKey{httproute.Namespace, serviceName, strconv.Itoa(int(servicePort))}
				tunnel, found := tunnels[key]
				if !found {
					targetAddr := fmt.Sprintf("%s.%s.%s:%d", serviceName, key.namespace, d.clusterDomain, servicePort)
					tunnel = ingressv1alpha1.Tunnel{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName:    fmt.Sprintf("%s-%d-", serviceName, servicePort),
							Namespace:       httproute.Namespace,
							OwnerReferences: nil, // fill owner references below
							Labels:          d.tunnelLabels(serviceName, servicePort),
						},
						Spec: ingressv1alpha1.TunnelSpec{
							ForwardsTo: targetAddr,
							Labels:     d.ngrokLabels(httproute.Namespace, serviceUID, serviceName, servicePort),
							BackendConfig: &ingressv1alpha1.BackendConfig{
								Protocol: protocol,
							},
							AppProtocol: appProtocol,
						},
					}
				}

				hasReference := false
				for _, ref := range tunnel.OwnerReferences {
					if ref.UID == httproute.UID {
						hasReference = true
						break
					}
				}
				if !hasReference {
					tunnel.OwnerReferences = append(tunnel.OwnerReferences, metav1.OwnerReference{
						APIVersion: httproute.APIVersion,
						Kind:       httproute.Kind,
						Name:       httproute.Name,
						UID:        httproute.UID,
					})
					slices.SortStableFunc(tunnel.OwnerReferences, func(i, j metav1.OwnerReference) int {
						return cmp.Compare(string(i.UID), string(j.UID))
					})
				}

				tunnels[key] = tunnel
			}
		}
	}
}

func (d *Driver) calculateIngressLoadBalancerIPStatus(ing *netv1.Ingress, c client.Reader) []netv1.IngressLoadBalancerIngress {
	ingressHosts := map[string]bool{}
	for _, rule := range ing.Spec.Rules {
		ingressHosts[rule.Host] = true
	}

	domains := &ingressv1alpha1.DomainList{}
	if err := c.List(context.Background(), domains); err != nil {
		d.log.Error(err, "failed to list domains")
		return []netv1.IngressLoadBalancerIngress{}
	}

	domainsByDomain := map[string]ingressv1alpha1.Domain{}
	for _, domain := range domains.Items {
		domainsByDomain[domain.Spec.Domain] = domain
	}

	status := []netv1.IngressLoadBalancerIngress{}

	for host := range ingressHosts {
		d, ok := domainsByDomain[host]
		if !ok {
			continue
		}

		var hostname string

		switch {
		// Custom domain
		case d.Status.CNAMETarget != nil:
			hostname = *d.Status.CNAMETarget
		// ngrok managed domain
		default:
			// Trim the wildcard prefix if it exists for ngrok managed domains
			hostname = strings.TrimPrefix(d.Status.Domain, "*.")
		}

		if hostname != "" {
			status = append(status, netv1.IngressLoadBalancerIngress{
				Hostname: hostname,
			})
		}
	}

	return status
}

func (d *Driver) getEdgeBackend(backendSvc netv1.IngressServiceBackend, namespace string) (string, int32, error) {
	service, servicePort, err := d.findBackendServicePort(backendSvc, namespace)
	if err != nil {
		return "", 0, err
	}

	return string(service.UID), servicePort.Port, nil
}

func (d *Driver) getEdgeBackendRef(backendRef gatewayv1.BackendRef, namespace string) (string, int32, error) {
	if backendRef.Namespace != nil && string(*backendRef.Namespace) != namespace {
		return "", 0, fmt.Errorf("namespace %s not supported", string(*backendRef.Namespace))
	}
	service, servicePort, err := d.findBackendRefServicePort(backendRef, namespace)
	if err != nil {
		return "", 0, err
	}

	return string(service.UID), servicePort.Port, nil
}

func (d *Driver) findBackendRefServicePort(backendRef gatewayv1.BackendRef, namespace string) (*corev1.Service, *corev1.ServicePort, error) {
	service, err := d.store.GetServiceV1(string(backendRef.Name), namespace)
	if err != nil {
		return nil, nil, err
	}
	servicePort, err := d.findBackendRefServicesPort(service, &backendRef)
	if err != nil {
		return nil, nil, err
	}

	return service, servicePort, nil
}

func (d *Driver) findBackendRefServicesPort(service *corev1.Service, backendRef *gatewayv1.BackendRef) (*corev1.ServicePort, error) {
	for _, port := range service.Spec.Ports {
		if (int32(*backendRef.Port) > 0 && port.Port == int32(*backendRef.Port)) || port.Name == string(backendRef.Name) {
			d.log.V(3).Info("Found matching port for service", "namespace", service.Namespace, "service", service.Name, "port.name", port.Name, "port.number", port.Port)
			return &port, nil
		}
	}
	return nil, fmt.Errorf("could not find matching port for service %s, backend port %v, name %s", service.Name, int32(*backendRef.Port), string(backendRef.Name))
}

func (d *Driver) getTunnelBackend(backendSvc netv1.IngressServiceBackend, namespace string) (string, int32, string, string, error) {
	service, servicePort, err := d.findBackendServicePort(backendSvc, namespace)
	if err != nil {
		return "", 0, "", "", err
	}

	protocol, err := d.getPortAnnotatedProtocol(service, servicePort.Name)
	if err != nil {
		return "", 0, "", "", err
	}

	appProtocol, err := d.getPortAppProtocol(service, servicePort)
	if err != nil {
		return "", 0, "", "", err
	}

	return string(service.UID), servicePort.Port, protocol, appProtocol, nil
}

func (d *Driver) getTunnelBackendFromGateway(backendRef gatewayv1.BackendRef, namespace string) (string, int32, string, string, error) {
	service, servicePort, err := d.findBackendRefServicePort(backendRef, namespace)
	if err != nil {
		return "", 0, "", "", err
	}

	protocol, err := d.getPortAnnotatedProtocol(service, servicePort.Name)
	if err != nil {
		return "", 0, "", "", err
	}

	appProtocol, err := d.getPortAppProtocol(service, servicePort)
	if err != nil {
		return "", 0, "", "", err
	}

	return string(service.UID), servicePort.Port, protocol, appProtocol, nil
}

func (d *Driver) findBackendServicePort(backendSvc netv1.IngressServiceBackend, namespace string) (*corev1.Service, *corev1.ServicePort, error) {
	service, err := d.store.GetServiceV1(backendSvc.Name, namespace)
	if err != nil {
		return nil, nil, err
	}

	servicePort, err := d.findServicesPort(service, backendSvc.Port)
	if err != nil {
		return nil, nil, err
	}

	return service, servicePort, nil
}

func (d *Driver) findServicesPort(service *corev1.Service, backendSvcPort netv1.ServiceBackendPort) (*corev1.ServicePort, error) {
	for _, port := range service.Spec.Ports {
		if (backendSvcPort.Number > 0 && port.Port == backendSvcPort.Number) || port.Name == backendSvcPort.Name {
			d.log.V(3).Info("Found matching port for service", "namespace", service.Namespace, "service", service.Name, "port.name", port.Name, "port.number", port.Port)
			return &port, nil
		}
	}
	return nil, fmt.Errorf("could not find matching port for service %s, backend port %v, name %s", service.Name, backendSvcPort.Number, backendSvcPort.Name)
}

func (d *Driver) getPortAnnotatedProtocol(service *corev1.Service, portName string) (string, error) {
	if service.Annotations != nil {
		annotation := service.Annotations["k8s.ngrok.com/app-protocols"]
		if annotation != "" {
			d.log.V(3).Info("Annotated app-protocols found", "annotation", annotation, "namespace", service.Namespace, "service", service.Name, "portName", portName)
			m := map[string]string{}
			err := json.Unmarshal([]byte(annotation), &m)
			if err != nil {
				return "", fmt.Errorf("could not parse protocol annotation: '%s' from: %s service: %s", annotation, service.Namespace, service.Name)
			}

			if protocol, ok := m[portName]; ok {
				d.log.V(3).Info("Found protocol for port name", "protocol", protocol, "namespace", service.Namespace, "service", service.Name)
				// only allow cases through where we are sure of intent
				switch upperProto := strings.ToUpper(protocol); upperProto {
				case "HTTP", "HTTPS":
					return upperProto, nil
				default:
					return "", fmt.Errorf("unhandled protocol annotation: '%s', must be 'HTTP' or 'HTTPS'. From: %s service: %s", upperProto, service.Namespace, service.Name)
				}
			}
		}
	}
	return "HTTP", nil
}

func (d *Driver) getPortAppProtocol(service *corev1.Service, port *corev1.ServicePort) (string, error) {
	if port.AppProtocol == nil {
		return "", nil
	}

	switch proto := *port.AppProtocol; proto {
	case "k8s.ngrok.com/http2", "kubernetes.io/h2c":
		return "http2", nil
	case "":
		return "", nil
	default:
		return "", fmt.Errorf("unsupported appProtocol: '%s', must be 'k8s.ngrok.com/http2', 'kubernetes.io/h2c' or ''. From: %s service: %s", proto, service.Namespace, service.Name)
	}
}

func (d *Driver) edgeLabels() map[string]string {
	return map[string]string{
		labelControllerNamespace: d.managerName.Namespace,
		labelControllerName:      d.managerName.Name,
	}
}

func (d *Driver) tunnelLabels(serviceName string, port int32) map[string]string {
	return map[string]string{
		labelControllerNamespace: d.managerName.Namespace,
		labelControllerName:      d.managerName.Name,
		labelService:             serviceName,
		labelPort:                strconv.Itoa(int(port)),
	}
}

// Generates a labels map for matching ngrok Routes to Agent Tunnels
func (d *Driver) ngrokLabels(namespace, serviceUID, serviceName string, port int32) map[string]string {
	return map[string]string{
		labelNamespace:  namespace,
		labelServiceUID: serviceUID,
		labelService:    serviceName,
		labelPort:       strconv.Itoa(int(port)),
	}
}

// MigrateKubernetesIngressControllerLabelsToNgrokOperator migrates the labels from the old Kubernetes Ingress Controller to the new ngrok operator labels
// so that the ngrok operator can take over management of items previously managed by the Kubernetes Ingress Controller.
// TODO: Delete this function after users have migrated from the ngrok Kubernetes Ingress Controller to the ngrok Operator.
func (d *Driver) MigrateKubernetesIngressControllerLabelsToNgrokOperator(ctx context.Context, k8sClient client.Client) error {
	typesToMigrate := []interface{}{
		&ingressv1alpha1.Domain{},
		&ingressv1alpha1.Tunnel{},
		&ingressv1alpha1.HTTPSEdge{},
	}

	for _, t := range typesToMigrate {
		objs, err := listObjectsForType(ctx, k8sClient, t)
		if err != nil {
			return err
		}

		for _, obj := range objs {

			name := obj.GetName()
			namespace := obj.GetNamespace()
			kind := obj.GetObjectKind().GroupVersionKind().Kind

			log := d.log.WithValues("name", name, "namespace", namespace, "kind", kind)

			labels := obj.GetLabels()

			controllerName, controllerNameOk := labels[labelControllerName]
			controllerNamespace, controllerNamespaceOk := labels[labelControllerNamespace]

			// If it doesn't have both of the controller name and namespace labels, skip it
			if !controllerNameOk || !controllerNamespaceOk {
				log.V(1).Info("Skipping object without controller name and namespace labels")
				continue
			}

			// If the controller name and namespace are the same as the current controller name and namespace, skip it
			if controllerName == d.managerName.Name && controllerNamespace == d.managerName.Namespace {
				log.V(1).Info("Skipping object with matching controller name and namespace labels")
				continue
			}

			// Deep copy the object so we can modify it.
			// We must also re-assign the labels to the new object so that
			// we don't modify the original object's labels.
			newObj := obj.DeepCopyObject().(client.Object)
			labels = newObj.GetLabels()

			// Now we know it has both labels and at least one of the two labels doesn't match
			// so we can update the labels to match what the new operator expects.
			labels[labelControllerName] = d.managerName.Name
			labels[labelControllerNamespace] = d.managerName.Namespace

			newObj.SetLabels(labels)

			if err := k8sClient.Patch(ctx, newObj, client.MergeFrom(obj)); err != nil {
				return err
			}
			log.V(1).Info("Migrated labels")
		}
	}
	return nil
}

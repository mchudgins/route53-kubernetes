package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/types"
	"github.com/aws/aws-sdk-go/aws"
	//	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/golang/glog"
)

type k8sClient struct {
	*client.Client
}

type serviceStatus struct {
	uid     types.UID
	dnsName string
	lb      api.LoadBalancerIngress
	hzId    string
}

var awsRegion = flag.String("region", "us-east-1", "AWS region")
var addr = flag.String("apiserver", "", "k8s server ip address (https://192.168.1.1)")
var user = flag.String("username", "", "apiserver username")
var pword = flag.String("password", "", "apiserver password")

func k8sClientFactory() *k8sClient {
	if len(*addr) > 0 && len(*user) > 0 && len(*pword) > 0 {
		config := client.Config{
			Host:     *addr,
			Username: *user,
			Password: *pword,
			Insecure: true,
		}
		return &k8sClient{client.NewOrDie(&config)}
	} else {
		kubernetesService := os.Getenv("KUBERNETES_SERVICE_HOST")
		if kubernetesService == "" {
			glog.Fatalf("Please specify the Kubernetes server with --server")
		}
		apiServer := fmt.Sprintf("https://%s:%s", kubernetesService, os.Getenv("KUBERNETES_SERVICE_PORT"))

		token, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err != nil {
			glog.Fatalf("No service account token found")
		}

		config := client.Config{
			Host:        apiServer,
			BearerToken: string(token),
			Insecure:    true,
		}

		c, err := client.New(&config)
		if err != nil {
			glog.Fatalf("Failed to make client: %v", err)
		}
		return &k8sClient{c}
	}
}

func (c *k8sClient) activeServices(selector string) (*api.ServiceList, error) {

	l, err := labels.Parse(selector)
	if err != nil {
		glog.Fatalf("Failed to parse selector %q: %v", selector, err)
	}

	return c.Services(api.NamespaceAll).List(l)
}

func main() {
	flag.Parse()
	glog.Info("Route53 Update Service")

	var previous struct {
		serviceCount int
	}
	glog.Infof("Connected to kubernetes @ %s", apiServer)

	k8s := k8sClientFactory()

	//	creds := credentials.NewCredentials(&credentials.EC2RoleProvider{})
	// Hardcode region to us-east-1 for now. Perhaps fetch through metadata service
	// curl http://169.254.169.254/latest/meta-data/placement/availability-zone
	awsConfig := aws.Config{
		//		Credentials: creds,
		Region: *awsRegion,
	}
	r53Api := route53.New(&awsConfig)
	elbApi := elb.New(&awsConfig)
	selector := "dns=route53"
	serviceMap := make(map[types.UID]*serviceStatus)

	glog.Infof("Starting Service Polling every 30s")
	for {
		services, err := k8s.activeServices(selector)
		if err != nil {
			glog.Fatalf("Failed to list pods: %v", err)
		}

		if len(services.Items) != previous.serviceCount {
			glog.Infof("Found %d DNS services in all namespaces with selector %q", len(services.Items), selector)
			previous.serviceCount = len(services.Items)
		}

		existsMap := make(map[types.UID]bool, len(services.Items))
		for i := range services.Items {
			s := &services.Items[i]
			hn, err := serviceHostname(s)
			if err != nil {
				glog.Warningf("Couldn't find hostname: %s", err)
				continue
			}

			domain, ok := s.ObjectMeta.Annotations["domainName"]
			if !ok {
				glog.Warningf("Domain name not set for %s", s.Name)
				continue
			}

			//glog.Infof("Creating DNS for %s service: %s -> %s", s.Name, hn, domain)
			crrsInput, err := createRoute53Upsert(r53Api, elbApi, domain, hn)
			if err != nil {
				continue
			}

			// before executing the change, see if the change is necessary
			// is the service listed in the map?
			// is the changeset the same as last time?
			existsMap[s.UID] = true
			previous := serviceMap[s.UID]
			target := serviceStatus{s.UID, domain,
				s.Status.LoadBalancer.Ingress[0],
				*crrsInput.ChangeBatch.Changes[0].ResourceRecordSet.AliasTarget.HostedZoneID}
			//glog.Infof("target:  %v", target)
			if previous == nil || *previous != target {
				glog.Infof("Creating DNS for %s service: %s -> %s", s.SelfLink, hn, domain)
				_, err = r53Api.ChangeResourceRecordSets(crrsInput)
				if err != nil {
					glog.Warningf("Failed to update record set: %v", err)
					continue
				}
				glog.Infof("Updated Route53 for %s successfully.", domain)
				serviceMap[s.UID] = &target
			}
		}

		// now, look for items we previously created and if
		// they weren't in the current services.Items list,
		// then remove them from route53
		for _, s := range serviceMap {
			if _, exists := existsMap[s.uid]; !exists {
				glog.Infof("Service %v can be removed from route53 (%s)", s.uid, s.dnsName)
				crrsInput, err := createRoute53Delete(r53Api, *s)
				if err == nil {
					_, err = r53Api.ChangeResourceRecordSets(crrsInput)
				}
				if err != nil {
					glog.Warningf("Failed to remove record set for %s:  %v", s.dnsName, err)
				}
				delete(serviceMap, s.uid)
			}
		}

		time.Sleep(30 * time.Second)
	}
}

func createRoute53Upsert(r53Api *route53.Route53, elbApi *elb.ELB, domain string, hn string) (*route53.ChangeResourceRecordSetsInput, error) {
	domainParts := strings.Split(domain, ".")
	segments := len(domainParts)
	tld := strings.Join(domainParts[segments-2:], ".")
	//	subdomain := strings.Join(domainParts[:segments-2], ".")

	elbName := strings.Split(hn, "-")[0]
	lbInput := &elb.DescribeLoadBalancersInput{
		LoadBalancerNames: []*string{
			&elbName,
		},
	}
	resp, err := elbApi.DescribeLoadBalancers(lbInput)
	if err != nil {
		glog.Warningf("Could not describe load balancer: %v", err)
		return nil, err
	}
	descs := resp.LoadBalancerDescriptions
	if len(descs) < 1 {
		glog.Warningf("No lb found for %s: %v", tld, err)
		return nil, err
	}
	if len(descs) > 1 {
		glog.Warningf("Multiple lbs found for %s: %v", tld, err)
		return nil, err
	}
	hzId := descs[0].CanonicalHostedZoneNameID

	listHostedZoneInput := route53.ListHostedZonesByNameInput{
		DNSName: &tld,
	}
	hzOut, err := r53Api.ListHostedZonesByName(&listHostedZoneInput)
	if err != nil {
		glog.Warningf("No zone found for %s: %v", tld, err)
		return nil, err
	}
	zones := hzOut.HostedZones
	if len(zones) < 1 {
		glog.Warningf("No zone found for %s", tld)
		return nil, err
	}
	// The AWS API may return more than one zone, the first zone should be the relevant one
	tldWithDot := fmt.Sprint(tld, ".")
	if *zones[0].Name != tldWithDot {
		glog.Warningf("Zone found %s does not match tld given %s", *zones[0].Name, tld)
		return nil, err
	}
	zoneId := *zones[0].ID
	zoneParts := strings.Split(zoneId, "/")
	zoneId = zoneParts[len(zoneParts)-1]

	at := route53.AliasTarget{
		DNSName:              &hn,
		EvaluateTargetHealth: aws.Boolean(false),
		HostedZoneID:         hzId,
	}
	rrs := route53.ResourceRecordSet{
		AliasTarget: &at,
		Name:        &domain,
		Type:        aws.String("A"),
	}
	change := route53.Change{
		Action:            aws.String("UPSERT"),
		ResourceRecordSet: &rrs,
	}
	batch := route53.ChangeBatch{
		Changes: []*route53.Change{&change},
		Comment: aws.String("Kubernetes Update to Service"),
	}
	crrsInput := route53.ChangeResourceRecordSetsInput{
		ChangeBatch:  &batch,
		HostedZoneID: &zoneId,
	}
	//glog.Infof("Created dns record set: tld=%s, subdomain=%s, zoneId=%s", tld, subdomain, zoneId)

	return &crrsInput, nil
}

func createRoute53Delete(r53Api *route53.Route53, s serviceStatus) (*route53.ChangeResourceRecordSetsInput, error) {
	domainParts := strings.Split(s.dnsName, ".")
	segments := len(domainParts)
	tld := strings.Join(domainParts[segments-2:], ".")
	subdomain := strings.Join(domainParts[:segments-2], ".")

	listHostedZoneInput := route53.ListHostedZonesByNameInput{
		DNSName: &tld,
	}
	hzOut, err := r53Api.ListHostedZonesByName(&listHostedZoneInput)
	if err != nil {
		glog.Warningf("No zone found for %s: %v", tld, err)
		return nil, err
	}
	zones := hzOut.HostedZones
	if len(zones) < 1 {
		glog.Warningf("No zone found for %s", tld)
		return nil, err
	}
	// The AWS API may return more than one zone, the first zone should be the relevant one
	tldWithDot := fmt.Sprint(tld, ".")
	if *zones[0].Name != tldWithDot {
		glog.Warningf("Zone found %s does not match tld given %s", *zones[0].Name, tld)
		return nil, err
	}
	zoneId := *zones[0].ID
	zoneParts := strings.Split(zoneId, "/")
	zoneId = zoneParts[len(zoneParts)-1]

	at := route53.AliasTarget{
		DNSName:              &s.lb.Hostname,
		EvaluateTargetHealth: aws.Boolean(false),
		HostedZoneID:         &s.hzId,
	}
	rrs := route53.ResourceRecordSet{
		AliasTarget: &at,
		Name:        &s.dnsName,
		Type:        aws.String("A"),
	}
	change := route53.Change{
		Action:            aws.String("DELETE"),
		ResourceRecordSet: &rrs,
	}
	batch := route53.ChangeBatch{
		Changes: []*route53.Change{&change},
		Comment: aws.String("Kubernetes Update to Service"),
	}
	crrsInput := route53.ChangeResourceRecordSetsInput{
		ChangeBatch:  &batch,
		HostedZoneID: &zoneId,
	}
	glog.Infof("Created dns delete record set: tld=%s, subdomain=%s, zoneId=%s", tld, subdomain, zoneId)

	return &crrsInput, nil
}

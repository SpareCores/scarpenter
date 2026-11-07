/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
)

// quotaClass is one vCPU quota class with its on-demand and spot quota codes
// (overview section 11.8). The fake keeps its own copy of the table, written
// from the verified facts, so that the driver's table is checked against an
// independent one.
type quotaClass struct{ od, spot string }

var classes = map[string]quotaClass{
	"standard": {"L-1216C47A", "L-34B43A08"},
	"f":        {"L-74FC7D96", "L-88CF9481"},
	"g":        {"L-DB2E81BA", "L-3819A6DF"},
	"inf":      {"L-1945791B", "L-B5D1601B"},
	"p":        {"L-417A185B", "L-7212CCBC"},
	"x":        {"L-7295265B", "L-E3A00192"},
	"trn":      {"L-2C3B7624", "L-6B0D517C"},
	"dl":       {"L-6E869C2A", "L-85EED4F7"},
	"hpc":      {"L-F7808C92", ""},
	"highmem":  {"L-43DA4232", ""},
}

// defaultQuotas are the defaults of a new account: five Standard vCPUs and
// nothing for the other classes.
var defaultQuotas = func() map[string]float64 {
	m := map[string]float64{}
	for name, c := range classes {
		v := 0.0
		if name == "standard" {
			v = 5
		}
		m[c.od] = v
		if c.spot != "" {
			m[c.spot] = v
		}
	}
	return m
}()

// classOfType maps an instance type to its quota class by the family rule.
func classOfType(typ string) string {
	fam, _, _ := strings.Cut(typ, ".")
	for _, p := range []string{"mac", "hpc", "dl", "trn", "inf", "vt"} {
		if strings.HasPrefix(fam, p) {
			switch p {
			case "vt":
				return "g"
			case "mac":
				return ""
			}
			return p
		}
	}
	switch fam[0] {
	case 'g':
		return "g"
	case 'f', 'p', 'x':
		return string(fam[0])
	case 'u':
		return "highmem"
	case 'a', 'c', 'd', 'h', 'i', 'm', 'r', 't', 'z':
		return "standard"
	}
	return ""
}

// usedVCPUs counts the vCPUs the service counts against the quota code: the
// pending and running instances of the class and capacity type.
func (a *Account) usedVCPUs(r *region, code string) int32 {
	var n int32
	for _, i := range r.insts {
		if i.state != types.InstanceStateNamePending && i.state != types.InstanceStateNameRunning &&
			i.state != types.InstanceStateNameStopping && i.state != types.InstanceStateNameShuttingDown {
			continue
		}
		c := classes[classOfType(i.typ)]
		want := c.od
		if i.lifecycle == "spot" {
			want = c.spot
		}
		if want == code {
			t, _ := a.typeOf(i.typ)
			n += t.VCPUs
		}
	}
	return n
}

// quotaCheck refuses a launch that would exceed the vCPU quota of its class.
func (a *Account) quotaCheck(r *region, in *ec2.RunInstancesInput, typ InstanceType, inst *instance) error {
	c, ok := classes[classOfType(typ.Name)]
	if !ok {
		return apiError("Unsupported", fmt.Sprintf("the instance type %s cannot be launched", typ.Name))
	}
	spot := in.InstanceMarketOptions != nil && in.InstanceMarketOptions.MarketType == types.MarketTypeSpot
	code, errCode := c.od, "VcpuLimitExceeded"
	if spot {
		code, errCode = c.spot, "MaxSpotInstanceCountExceeded"
	}
	if code == "" {
		return apiError("InvalidParameterCombination", fmt.Sprintf("%s has no spot capacity", typ.Name))
	}
	limit := r.quotas[code]
	if float64(a.usedVCPUs(r, code)+typ.VCPUs) > limit {
		return apiError(errCode, "You have requested more vCPU capacity than your current vCPU limit of "+
			fmt.Sprint(limit)+" allows for the instance bucket that the specified instance type belongs to.")
	}
	_ = inst
	return nil
}

// Quotas is the Service Quotas API of one region.
type Quotas struct {
	a      *Account
	region string
}

// Quotas returns the Service Quotas client of the region.
func (a *Account) Quotas(region string) *Quotas { return &Quotas{a: a, region: region} }

func (q *Quotas) begin(ctx context.Context, op string, in any) (*region, error) {
	q.a.mu.Lock()
	if err := q.a.begin(ctx, op, in); err != nil {
		q.a.mu.Unlock()
		return nil, err
	}
	r, err := q.a.regionOf(q.region)
	if err != nil {
		q.a.mu.Unlock()
		return nil, err
	}
	return r, nil
}

// GetServiceQuota implements ServiceQuotasAPI.
func (q *Quotas) GetServiceQuota(ctx context.Context, in *servicequotas.GetServiceQuotaInput, _ ...func(*servicequotas.Options),
) (*servicequotas.GetServiceQuotaOutput, error) {
	r, err := q.begin(ctx, "GetServiceQuota", in)
	if err != nil {
		return nil, err
	}
	defer q.a.mu.Unlock()
	if deref(in.ServiceCode) != "ec2" {
		return nil, apiError("NoSuchResourceException", "unknown service")
	}
	v, ok := r.quotas[deref(in.QuotaCode)]
	if !ok {
		return nil, apiError("NoSuchResourceException", "the quota does not exist")
	}
	return &servicequotas.GetServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{
		QuotaCode: in.QuotaCode, ServiceCode: in.ServiceCode, Value: &v, GlobalQuota: false}}, nil
}

// ListServiceQuotas implements ServiceQuotasAPI.
func (q *Quotas) ListServiceQuotas(ctx context.Context, in *servicequotas.ListServiceQuotasInput, _ ...func(*servicequotas.Options),
) (*servicequotas.ListServiceQuotasOutput, error) {
	r, err := q.begin(ctx, "ListServiceQuotas", in)
	if err != nil {
		return nil, err
	}
	defer q.a.mu.Unlock()
	var out []sqtypes.ServiceQuota
	for _, code := range sortedKeys(r.quotas) {
		v := r.quotas[code]
		out = append(out, sqtypes.ServiceQuota{QuotaCode: new(code), ServiceCode: in.ServiceCode, Value: &v})
	}
	// Two pages, to exercise the driver's pagination.
	start := 0
	if in.NextToken != nil {
		fmt.Sscanf(*in.NextToken, "%d", &start) //nolint:errcheck // a bad token starts at 0
	}
	end := min(start+10, len(out))
	var next *string
	if end < len(out) {
		next = new(fmt.Sprint(end))
	}
	return &servicequotas.ListServiceQuotasOutput{Quotas: slices.Clone(out[start:end]), NextToken: next}, nil
}

// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package dao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/render"
	"github.com/derailed/k9s/internal/slogs"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	StatusOK       = "OK"
	DegradedStatus = "DEGRADED"
)

var resList = []*client.GVR{
	client.PodGVR,
	client.SvcGVR,
	client.DsGVR,
	client.StsGVR,
	client.DpGVR,
	client.RsGVR,
}

// Workload tracks a select set of resources in a given namespace.
type Workload struct {
	Table
}

func (w *Workload) Delete(ctx context.Context, path string, propagation *metav1.DeletionPropagation, grace Grace) error {
	gvr, _ := ctx.Value(internal.KeyGVR).(*client.GVR)
	ns, n := client.Namespaced(path)
	auth, err := w.Client().CanI(ns, gvr, n, []string{client.DeleteVerb})
	if err != nil {
		return err
	}
	if !auth {
		return fmt.Errorf("user is not authorized to delete %s", path)
	}

	var gracePeriod *int64
	if grace != DefaultGrace {
		gracePeriod = (*int64)(&grace)
	}
	opts := metav1.DeleteOptions{
		PropagationPolicy:  propagation,
		GracePeriodSeconds: gracePeriod,
	}

	ctx, cancel := context.WithTimeout(ctx, w.Client().Config().CallTimeout())
	defer cancel()

	d, err := w.Client().DynDial()
	if err != nil {
		return err
	}
	dial := d.Resource(gvr.GVR())
	if client.IsClusterScoped(ns) {
		return dial.Delete(ctx, n, opts)
	}

	return dial.Namespace(ns).Delete(ctx, n, opts)
}

func (a *Workload) fetch(ctx context.Context, gvr *client.GVR, ns string) (*metav1.Table, error) {
	a.gvr = gvr
	oo, err := a.Table.List(ctx, ns)
	if err != nil {
		return nil, err
	}
	if len(oo) == 0 {
		return nil, fmt.Errorf("no table found for gvr: %s", gvr)
	}
	tt, ok := oo[0].(*metav1.Table)
	if !ok {
		return nil, errors.New("not a metav1.Table")
	}

	return tt, nil
}

// List fetch workloads.
func (a *Workload) List(ctx context.Context, ns string) ([]runtime.Object, error) {
	oo := make([]runtime.Object, 0, 100)
	for _, gvr := range resList {
		table, err := a.fetch(ctx, gvr, ns)
		if err != nil {
			return nil, err
		}
		var (
			ns string
			ts metav1.Time
		)
		for _, r := range table.Rows {
			if obj := r.Object.Object; obj != nil {
				if m, err := meta.Accessor(obj); err == nil {
					ns, ts = m.GetNamespace(), m.GetCreationTimestamp()
				}
			} else {
				var m metav1.PartialObjectMetadata
				if err := json.Unmarshal(r.Object.Raw, &m); err == nil {
					ns, ts = m.GetNamespace(), m.CreationTimestamp
				}
			}
			stat := status(gvr, &r, table.ColumnDefinitions)
			oo = append(oo, &render.WorkloadRes{Row: metav1.TableRow{Cells: []any{
				gvr.String(),
				ns,
				r.Cells[indexOf("Name", table.ColumnDefinitions)],
				stat,
				readiness(gvr, &r, table.ColumnDefinitions),
				validity(stat),
				ts,
			}}})
		}
	}

	return oo, nil
}

// Helpers...

func readiness(gvr *client.GVR, r *metav1.TableRow, h []metav1.TableColumnDefinition) string {
	switch gvr {
	case client.PodGVR, client.DpGVR, client.StsGVR:
		return r.Cells[indexOf("Ready", h)].(string)
	case client.RsGVR, client.DsGVR:
		c := r.Cells[indexOf("Ready", h)].(int64)
		d := r.Cells[indexOf("Desired", h)].(int64)
		return fmt.Sprintf("%d/%d", c, d)
	case client.SvcGVR:
		return ""
	}

	return render.NAValue
}

func status(gvr *client.GVR, r *metav1.TableRow, h []metav1.TableColumnDefinition) string {
	switch gvr {
	case client.PodGVR:
		if status := r.Cells[indexOf("Status", h)]; status == render.PhaseCompleted {
			return StatusOK
		} else if !isReady(r.Cells[indexOf("Ready", h)].(string)) || status != render.PhaseRunning {
			return DegradedStatus
		}
	case client.DpGVR, client.StsGVR:
		if !isReady(r.Cells[indexOf("Ready", h)].(string)) {
			return DegradedStatus
		}
	case client.RsGVR, client.DsGVR:
		rd, ok1 := r.Cells[indexOf("Ready", h)].(int64)
		de, ok2 := r.Cells[indexOf("Desired", h)].(int64)
		if ok1 && ok2 {
			if !isReady(fmt.Sprintf("%d/%d", rd, de)) {
				return DegradedStatus
			}
			break
		}
		rds, oks1 := r.Cells[indexOf("Ready", h)].(string)
		des, oks2 := r.Cells[indexOf("Desired", h)].(string)
		if oks1 && oks2 {
			if !isReady(fmt.Sprintf("%s/%s", rds, des)) {
				return DegradedStatus
			}
		}
	case client.SvcGVR:
	default:
		return render.MissingValue
	}

	return StatusOK
}

func validity(status string) string {
	if status != "DEGRADED" {
		return ""
	}

	return status
}

func isReady(s string) bool {
	tt := strings.Split(s, "/")
	if len(tt) != 2 {
		return false
	}
	r, err := strconv.Atoi(tt[0])
	if err != nil {
		slog.Error("Invalid ready count",
			slogs.Error, err,
			slogs.Count, tt[0],
		)
		return false
	}
	c, err := strconv.Atoi(tt[1])
	if err != nil {
		slog.Error("invalid expected count: %q",
			slogs.Error, err,
			slogs.Count, tt[1],
		)
		return false
	}

	if c == 0 {
		return true
	}
	return r == c
}

func indexOf(n string, defs []metav1.TableColumnDefinition) int {
	for i, d := range defs {
		if d.Name == n {
			return i
		}
	}

	return -1
}

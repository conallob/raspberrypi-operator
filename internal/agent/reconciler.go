// Package agent implements the per-node reconciler that stages bootloader
// updates and reboots the node when the controller says it may.
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/eeprom"
	"github.com/conallob/raspberrypi-operator/internal/node"
)

// Reconciler drives the NodeBootloader for one node.
type Reconciler struct {
	Client client.Client
	// Reader reads the Node (for the new object's owner reference) without a
	// cache; the agent's cache is limited to its own NodeBootloader. Defaults
	// to Client.
	Reader   client.Reader
	NodeName string
	BootFS   string
	Probe    func() (node.Info, error)
	Fetch    Fetcher
	Rebooter node.Rebooter
	Now      func() time.Time
	// Interval is how often the node is re-probed.
	Interval time.Duration
}

// The agent's RBAC is config/rbac/bindings.yaml (and the chart's rbac.yaml):
//   nodebootloaders: get, list, watch, create (its own node's object only, which
//   the admission policy enforces); nodebootloaders/status: get, update, patch;
//   nodes: get (to own the object it creates).

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != r.NodeName {
		return ctrl.Result{}, nil
	}
	var nb rpiv1.NodeBootloader
	if err := r.Client.Get(ctx, req.NamespacedName, &nb); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted (or never created): put it back, so the node stays visible.
			return ctrl.Result{}, r.ensureObject(ctx)
		}
		return ctrl.Result{}, err
	}
	logger := log.FromContext(ctx)
	orig := nb.DeepCopy()

	info, err := r.Probe()
	if err != nil {
		nb.Status.TargetHash = nb.Spec.TargetHash()
		if nb.Spec.Target == nil {
			nb.Status.Phase = rpiv1.PhaseUnmanaged
		} else {
			nb.Status.Phase = rpiv1.PhaseBlocked
		}
		nb.Status.Message = err.Error()
		return r.finish(ctx, orig, &nb, nil)
	}

	reboot := r.evaluate(ctx, &nb, info)
	if nb.Status.Phase != orig.Status.Phase {
		logger.Info("phase changed", "from", orig.Status.Phase, "to", nb.Status.Phase, "message", nb.Status.Message)
		if !rpiv1.CanTransition(orig.Status.Phase, nb.Status.Phase) {
			// A bug in the state machine, not something to wedge a node over.
			logger.Error(nil, "phase transition is not in the state machine",
				"from", orig.Status.Phase, "to", nb.Status.Phase)
		}
	}
	return r.finish(ctx, orig, &nb, reboot)
}

// ensureObject creates this node's NodeBootloader, empty, if it does not exist:
// that is what makes the node visible before any policy selects it. Nothing is
// created on a board the operator does not support (an arm64 server, say).
func (r *Reconciler) ensureObject(ctx context.Context) error {
	logger := log.FromContext(ctx)
	if _, err := r.Probe(); err != nil {
		logger.Info("not creating a NodeBootloader: this node cannot be managed", "reason", err.Error())
		return nil
	}
	nb := &rpiv1.NodeBootloader{ObjectMeta: metav1.ObjectMeta{Name: r.NodeName}}
	// Owned by the Node, so the object goes when the node does.
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	var n corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: r.NodeName}, &n); err != nil {
		logger.Info("creating the NodeBootloader without an owner reference", "reason", err.Error())
	} else if err := controllerutil.SetOwnerReference(&n, nb, r.Client.Scheme()); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, nb); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	logger.Info("created the NodeBootloader for this node; it is Unmanaged until a policy selects it")
	return nil
}

// Bootstrap creates the node's NodeBootloader at startup, retrying until the API
// server accepts it. It is a manager Runnable: without it nothing would ever
// trigger a reconcile for an object that does not exist yet.
func (r *Reconciler) Bootstrap(ctx context.Context) error {
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := r.ensureObject(ctx); err != nil {
			log.FromContext(ctx).Error(err, "creating the NodeBootloader; will retry")
			return false, nil
		}
		return true, nil
	})
}

// finish persists status and only then performs the reboot, so a reboot is
// never issued unless the node's intent to reboot is already recorded.
func (r *Reconciler) finish(ctx context.Context, orig, nb *rpiv1.NodeBootloader, reboot func() error) (ctrl.Result, error) {
	now := metav1.NewTime(r.Now())
	nb.Status.LastProbeTime = &now
	if !equality.Semantic.DeepEqual(orig.Status.DeepCopy(), nb.Status.DeepCopy()) || orig.Status.LastProbeTime == nil ||
		now.Sub(orig.Status.LastProbeTime.Time) > time.Minute {
		if err := r.Client.Status().Patch(ctx, nb, client.MergeFrom(orig)); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}
	if reboot != nil {
		log.FromContext(ctx).Info("rebooting to apply bootloader update", "target", nb.Status.TargetHash)
		if err := reboot(); err != nil {
			return ctrl.Result{}, fmt.Errorf("reboot: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: r.Interval}, nil
}

func (r *Reconciler) block(nb *rpiv1.NodeBootloader, format string, args ...any) func() error {
	nb.Status.Phase = rpiv1.PhaseBlocked
	nb.Status.Message = fmt.Sprintf(format, args...)
	return nil
}

// evaluate updates nb.Status for the current probe, stages an update if one is
// approved, and returns a reboot function if the node should reboot now.
func (r *Reconciler) evaluate(ctx context.Context, nb *rpiv1.NodeBootloader, info node.Info) func() error {
	target := nb.Spec.Target
	hash := nb.Spec.TargetHash()
	st := &nb.Status

	st.Model, st.Chip, st.BuildTimestamp = info.Model, info.Chip, info.BuildTimestamp
	st.BootDevice, st.BootID, st.TargetHash = info.BootDevice, info.BootID, hash
	st.BootOrder = ""
	if info.ConfigErr == nil {
		if v, ok := eeprom.ParseConfig(info.Config).Get(eeprom.KeyBootOrder); ok {
			st.BootOrder = describeBootOrder(v)
		}
	}

	// No policy selects this node: report what is there and do nothing. The only
	// thing cleaned up is an update this agent staged for a policy that has since
	// let go of the node; a file somebody else staged is never touched.
	if target == nil {
		if st.StagedHash != "" {
			_ = node.Unstage(r.BootFS)
		}
		st.StagedHash, st.RebootingFromBootID, st.FailedHash = "", "", ""
		st.Phase = rpiv1.PhaseUnmanaged
		st.Message = "no BootloaderPolicy selects this node; observing only"
		if info.ConfigErr != nil {
			st.Message += " (the running bootloader config cannot be read: " + info.ConfigErr.Error() + ")"
		}
		return nil
	}
	// Anything left from a previous target is not this target's problem.
	if st.StagedHash != "" && st.StagedHash != hash {
		_ = node.Unstage(r.BootFS)
		st.StagedHash = ""
	}
	// Did we just come back from a reboot we asked for? Judged below.
	rebooted := st.RebootingFromBootID != "" && st.RebootingFromBootID != info.BootID
	if rebooted {
		st.RebootingFromBootID = ""
	}

	desired, err := desiredSettings(target)
	if err != nil {
		return r.block(nb, "%v", err)
	}
	img, ok := target.Firmware.ImageFor(info.Chip)
	if !ok {
		return r.block(nb, "the policy has no firmware image for %s", info.Chip)
	}
	fwDrift := info.BuildTimestamp != img.BuildTimestamp
	cfgDrift := false
	if len(desired) > 0 {
		if info.ConfigErr != nil {
			return r.block(nb, "%v", info.ConfigErr)
		}
		cfgDrift = !eeprom.ParseConfig(info.Config).Satisfied(desired)
	}

	if !fwDrift && !cfgDrift {
		if err := node.Unstage(r.BootFS); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "removing leftover staged update")
		}
		st.Phase, st.Message, st.StagedHash, st.FailedHash = rpiv1.PhaseUpToDate, "", "", ""
		return nil
	}

	// The node still differs from the target.
	if rebooted && st.StagedHash == hash {
		_ = node.Unstage(r.BootFS)
		st.StagedHash = ""
		st.FailedHash = hash
		st.Phase = rpiv1.PhaseFailed
		st.Message = fmt.Sprintf("rebooted but still differs from target (running build %d, want %d; config satisfied: %t); not retrying",
			info.BuildTimestamp, img.BuildTimestamp, !cfgDrift)
		return nil
	}
	if st.FailedHash == hash {
		st.Phase = rpiv1.PhaseFailed
		return nil // sticky until the target changes
	}
	st.FailedHash = ""

	if reason := preflight(target, img, info, r.BootFS); reason != "" {
		if st.StagedHash != "" {
			_ = node.Unstage(r.BootFS)
			st.StagedHash = ""
		}
		return r.block(nb, "%s", reason)
	}

	if nb.Spec.ApprovedHash != hash {
		st.Phase = rpiv1.PhasePending
		st.Message = "update available; waiting for the controller to approve this node"
		return nil
	}

	// Approved. Reuse a staged update only if it is intact.
	if st.StagedHash == hash {
		if _, ok, err := node.StagedDigest(r.BootFS); err != nil || !ok {
			ctrl.LoggerFrom(ctx).Info("staged update missing or damaged; restaging", "error", err)
			st.StagedHash = ""
		}
	}
	if st.StagedHash != hash {
		if msg := r.stage(ctx, nb, info, img, desired); msg != "" {
			return r.block(nb, "%s", msg)
		}
		st.StagedHash = hash
	}

	if nb.Spec.RebootHash == hash {
		st.Phase = rpiv1.PhaseRebooting
		st.Message = "rebooting to apply the bootloader update"
		st.RebootingFromBootID = info.BootID
		return r.Rebooter.Reboot
	}
	st.Phase = rpiv1.PhaseStaged
	st.Message = "update staged; waiting for the node to be drained and cleared to reboot"
	return nil
}

// typedKeys maps the typed config fields to their bootloader keys.
func typedKeys(c *rpiv1.BootloaderConfig) map[string]*bool {
	return map[string]*bool{
		"BOOT_UART":               c.BootUART,
		"WAKE_ON_GPIO":            c.WakeOnGPIO,
		"POWER_OFF_ON_HALT":       c.PowerOffOnHalt,
		"NET_INSTALL_AT_POWER_ON": c.NetInstallAtPowerOn,
	}
}

// bootOrder converts the target's boot order, if any.
func bootOrder(c *rpiv1.BootloaderConfig) (eeprom.BootOrder, bool, error) {
	if len(c.BootOrder) == 0 {
		return nil, false, nil
	}
	names := make([]string, len(c.BootOrder))
	for i, s := range c.BootOrder {
		names[i] = string(s)
	}
	o, err := eeprom.BootOrderFromNames(names)
	return o, true, err
}

// describeBootOrder renders a running BOOT_ORDER value as boot source names,
// falling back to the raw value if it uses a mode the operator doesn't know.
func describeBootOrder(raw string) string {
	o, err := eeprom.ParseBootOrder(raw)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return strings.Join(o.Names(), ",")
}

// desiredSettings flattens the target's config into bootloader key/values.
// The CRD already rejects the conflicts checked here; they are re-checked
// because the agent must not trust that the CRD schema it was installed with
// is current.
func desiredSettings(t *rpiv1.TargetSpec) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range t.Config.Settings {
		switch k {
		case eeprom.KeyBootOrder:
			return nil, fmt.Errorf("set BOOT_ORDER with config.bootOrder, not config.settings")
		case "ENABLE_SELF_UPDATE":
			return nil, fmt.Errorf("ENABLE_SELF_UPDATE cannot be managed: the update mechanism depends on it")
		}
		out[k] = string(v)
	}
	for k, b := range typedKeys(&t.Config) {
		if b == nil {
			continue
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%s is set both by its typed field and in config.settings", k)
		}
		out[k] = "0"
		if *b {
			out[k] = "1"
		}
	}
	o, ok, err := bootOrder(&t.Config)
	if err != nil {
		return nil, err
	}
	if ok {
		out[eeprom.KeyBootOrder] = o.Canonical()
	}
	return out, nil
}

// preflight runs every check that can be done without writing anything. It
// returns a reason to refuse, or "" if the update may proceed.
func preflight(t *rpiv1.TargetSpec, img rpiv1.ImageSource, info node.Info, bootfs string) string {
	if r := node.SelfUpdateBlocked(info, bootfs); r != "" {
		return r
	}
	if info.BuildTimestamp > img.BuildTimestamp && !t.Safety.AllowDowngrade {
		return fmt.Sprintf("target build %d is older than the running build %d; set safety.allowDowngrade to permit",
			img.BuildTimestamp, info.BuildTimestamp)
	}
	o, ok, err := bootOrder(&t.Config)
	if err != nil {
		return err.Error()
	}
	if !ok {
		return ""
	}
	if t.Safety.AllowUnsafeBootOrder {
		// Still refuse an order that cannot boot anything at all.
		if err := o.Validate(nil); err != nil {
			return err.Error()
		}
		return ""
	}
	modes := node.ModesFor(info.BootDevice)
	if modes == nil {
		return fmt.Sprintf("cannot tell which device this node boots from (%s), so BOOT_ORDER cannot be checked; set safety.allowUnsafeBootOrder to override", info.BootDevice)
	}
	if err := o.Validate(modes); err != nil {
		return err.Error()
	}
	return ""
}

// stage downloads the image, edits its config to merge the node's current
// config with the desired settings, and writes the update files. It returns a
// reason on failure; nothing is left staged in that case.
func (r *Reconciler) stage(ctx context.Context, nb *rpiv1.NodeBootloader, info node.Info, src rpiv1.ImageSource, desired map[string]string) string {
	raw, err := r.Fetch.Fetch(ctx, src.URL, src.SHA256)
	if err != nil {
		return err.Error()
	}
	img, err := eeprom.ParseImage(raw)
	if err != nil {
		return err.Error()
	}
	// Start from the running config so settings we don't manage survive; fall
	// back to the image's own config only if there is nothing to merge.
	base := info.Config
	if info.ConfigErr != nil {
		if len(desired) > 0 {
			return info.ConfigErr.Error()
		}
		if base, err = img.Config(); err != nil {
			return err.Error()
		}
	}
	merged, err := eeprom.ParseConfig(base).Apply(desired)
	if err != nil {
		return err.Error()
	}
	if err := img.SetConfig(merged.String()); err != nil {
		return err.Error()
	}
	out := img.Bytes()
	sig := eeprom.Sig(out, r.Now(), node.SoC(info.Chip))
	if err := node.Stage(r.BootFS, out, sig); err != nil {
		_ = node.Unstage(r.BootFS)
		return fmt.Sprintf("staging update: %v", err)
	}
	if _, ok, err := node.StagedDigest(r.BootFS); err != nil || !ok {
		_ = node.Unstage(r.BootFS)
		return fmt.Sprintf("staged update failed verification: %v", err)
	}
	return ""
}

// SetupWithManager registers the reconciler for this node's object only, and
// the startup step that creates that object.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(manager.RunnableFunc(r.Bootstrap)); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&rpiv1.NodeBootloader{}).
		Named("nodebootloader-agent").
		Complete(r)
}

package intelligence

import "testing"

func TestReadOnly(t *testing.T) {
	tests := []struct {
		command string
		ok      bool
		why     string
	}{
		{command: "kubectl get pods -n shop", ok: true},
		{command: "kubectl -n shop describe pod/billing-5d4f-x2x4z", ok: true},
		{command: "kubectl --context=prod logs pod/billing-5d4f-x2x4z -c main --previous", ok: true},
		{command: "kubectl events --for pod/billing-5d4f-x2x4z -n shop", ok: true},
		{command: "kubectl rollout status deploy/billing", ok: true},
		{command: "kubectl auth can-i list secrets -n shop", ok: true},
		{command: "kubectl get secrets -n shop", ok: true},
		{command: "kubectl get secret checkout-tls -o wide", ok: true},
		{command: "kubectl describe secret checkout-tls", ok: true},
		{command: "kubectl get configmap checkout-config -o yaml", ok: true},
		{command: "tug why backups.db.example.com/orders-nightly -n shop", ok: true},
		{command: "tug logs pod/billing-5d4f-x2x4z --previous -n shop", ok: true},
		{command: "tug tree db/orders", ok: true},
		{command: "tug describe pod/billing-5d4f-x2x4z", ok: true}, // runs as kubectl describe
		{command: "tug --cluster-selector env=staging get pods", ok: true},

		// tug's newer commands that only read, and the subcommands that do.
		{command: "tug check -A --pack acme", ok: true},
		{command: "tug diff deploy/api -f k8s/api.yaml", ok: true},
		{command: "tug drift --against payments-staging -n shop", ok: true},
		{command: "tug watch deploy/api --until ready --timeout 5m", ok: true},
		{command: "tug gitops explain kustomization/apps -n flux-system", ok: true},
		{command: "tug gitops deps helmrelease/web", ok: true},
		{command: "tug reconcile trace deploy/api", ok: true},
		{command: "tug reconcile -n shop events db/orders", ok: true},
		{command: "tug api deprecations", ok: true},
		{command: "tug crd schema certificates.cert-manager.io", ok: true},
		{command: "tug team show platform", ok: true},
		{command: "tug settings get intelligence.model", ok: true},
		{command: "tug intelligence status", ok: true},
		{command: "tug plugins search gpu", ok: true},
		{command: "tug tunnel test eks-private", ok: true},
		{command: "tug pin show", ok: true},
		{command: "tug daemon status", ok: true},
		{command: "tug pf ls", ok: true},
		{command: "tug current", ok: true},
		{command: "tug clusters -l env=prod", ok: true},
		{command: "tug gitops reconcile kustomization/apps", why: "tug gitops reconcile is not a command that only reads"},
		{command: "tug team add platform https://teams.example/t.yaml", why: "tug team add is not a command that only reads"},
		{command: "tug settings set intelligence.enabled false", why: "tug settings set is not a command that only reads"},
		{command: "tug intelligence enable openai", why: "tug intelligence enable is not a command that only reads"},
		{command: "tug plugin install acme", why: "tug plugin install is not a command that only reads"},
		{command: "tug pf api 8080", why: "tug pf api is not a command that only reads"},
		{command: "tug daemon stop", why: "tug daemon stop is not a command that only reads"},
		{command: "tug profile use payments", why: "tug profile use is not a command that only reads"},
		{command: "tug bookmark open api", why: "tug bookmark open is not a command that only reads"},
		{command: "tug api", why: "tug api is not a command that only reads"},
		{command: "tug support bundle deploy/api", why: "tug support is not a command that only reads"},
		{command: "tug open deploy/api", why: "tug open is not a command that only reads"},
		{command: "tug c prod", why: "tug c is not a command that only reads"},

		{command: "kubectl delete pod billing-5d4f-x2x4z", why: "kubectl delete is not a command that only reads"},
		{command: "kubectl -n shop scale deploy/billing --replicas=0", why: "kubectl scale is not a command that only reads"},
		{command: "kubectl rollout restart deploy/billing", why: "kubectl rollout restart is not a command that only reads"},
		{command: "kubectl auth reconcile -f rbac.yaml", why: "kubectl auth reconcile is not a command that only reads"},
		{command: "kubectl exec -it pod/x -- sh", why: "kubectl exec is not a command that only reads"},
		{command: "kubectl port-forward svc/db 5432", why: "kubectl port-forward is not a command that only reads"},
		{command: "kubectl get secret orders-credentials -o yaml", why: "it would print a Secret's data"},
		{command: "kubectl get secrets/orders-credentials -ojsonpath={.data.password}", why: "it would print a Secret's data"},
		{command: "kubectl get pods,secrets --output=json", why: "it would print a Secret's data"},
		{command: "kubectl get --raw /api/v1/namespaces/shop/secrets", why: "it would print a Secret's data"},
		{command: "kubectl get pods --token=abc", why: "it passes a credential on the command line"},
		{command: "kubectl get pods | grep billing", why: "it uses the shell"},
		{command: "kubectl get pods; kubectl delete pod x", why: "it uses the shell"},
		{command: "kubectl get pods $(whoami)", why: "it uses the shell"},
		{command: "kubectl get pods > pods.txt", why: "it uses the shell"},
		{command: "helm uninstall billing", why: "tug keeps only kubectl and tug commands"},
		{command: "tug restart billing", why: "tug restart is not a command that only reads"},
		{command: "tug unlock 15m", why: "tug unlock is not a command that only reads"},
		{command: "tug why billing --deep", why: "it would send data to a model provider again"},
		{command: "tug edit deploy/billing", why: "kubectl edit is not a command that only reads"},
		{command: "kubectl -n shop", why: "it names no command"},
		{command: "  ", why: "it is empty"},
		{command: "kubectl get secret [secret 1] -n shop", why: "it holds a placeholder for a redacted value"},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			ok, why := ReadOnly(tt.command)
			if ok != tt.ok || why != tt.why {
				t.Errorf("ReadOnly(%q) = %v, %q; want %v, %q", tt.command, ok, why, tt.ok, tt.why)
			}
		})
	}
}

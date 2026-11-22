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

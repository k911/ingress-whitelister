# Kubernetes Ingress Whitelister for Traefik
Used to automatically add or update proper whitelist source ranges annotation on ingresses having `ingress-whitelister.ingress.kubernetes.io/whitelist-name` annotation.
Whitelists are defined as key=value pairs in Kubernetes ConfigMap resource, and also automatically updated upon change.

## Kubernetes

Current k8s libraries version: v1.12.5

```bash
go get k8s.io/api@kubernetes-1.12.5
go get k8s.io/apimachinery@kubernetes-1.12.5
go get k8s.io/client-go@kubernetes-1.12.5
```

## Quick start guide with traefik 

Info: This guide assumes you've already configured traefik ingress controller in your Kubernetes cluster.

1. Deploy controller with configured whitelists.

    ```bash
    echo "
    # values.yaml
    ingress:
      annotation:
        whitelistSourceRanges: traefik.ingress.kubernetes.io/whitelist-source-range

    whitelistsConfigMap:
      content: |
        whitelist1: 83.152.33.11/32, 10.10.2.0/16
    " > values.yaml

    helm upgrade --install ingress-whitelister ./helm -f values.yaml
    ```

2. Create ingress with `ingress-whitelister.ingress.kubernetes.io/whitelist-name: whitelist1`

3. Ingress should have defined annotation: `traefik.ingress.kubernetes.io/whitelist-source-range: 83.152.33.11/32, 10.10.2.0/16` and be accessible only from that source ranges, if traefik is properly configured.

4. (Optional) Edit configmap `ingress-whitelister` to update annotations in all ingresses.

5. Teardown 

    ```bash
    helm delete --purge ingress-whitelister
    ```
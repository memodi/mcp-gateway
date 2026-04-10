# MCP Gateway Console Plugin Component

This kustomize component deploys the OpenShift Console Plugin for MCP Gateway.

## Overview

The MCP Gateway Console Plugin provides a web UI integrated with the OpenShift Console for:
- Viewing registered MCP servers and their status
- Browsing available tools from each server
- Monitoring server health and validation status

## Resources Deployed

- **ConsolePlugin**: Registers the plugin with OpenShift Console
- **Deployment**: Runs the plugin server (nginx serving static files)
- **Service**: Exposes the plugin server with TLS (cert managed by service-serving-cert-secret)
- **ServiceAccount**: Service account for the plugin pods
- **ConfigMap**: Nginx configuration for serving plugin files

## Usage

### Include in Your Kustomization

To deploy the console plugin along with the MCP Gateway controller, add this component to your kustomization:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - ../path/to/mcp-gateway/base

components:
  - ../path/to/mcp-gateway/components/console-plugin
```

### Deploy with Make

From the mcp-gateway repository:

```bash
# Deploy everything including console plugin
make deploy-with-console-plugin

# Or customize your deployment
kustomize build config/mcp-system | kubectl apply -f -
```

### Verify Deployment

```bash
# Check plugin deployment
kubectl -n mcp-system get deployment mcp-gateway-console-plugin
kubectl -n mcp-system get pods -l app=mcp-gateway-console-plugin

# Check ConsolePlugin resource
kubectl get consoleplugin mcp-gateway-console-plugin

# Check if plugin is enabled in console
kubectl get console.operator.openshift.io cluster -o jsonpath='{.spec.plugins}'
```

## Plugin Registration

The ConsolePlugin resource is automatically created, but the plugin must be enabled in the OpenShift Console.

### Automatic Enablement

The console plugin will be automatically discovered by OpenShift Console. To enable it:

```bash
# Enable the plugin via console.operator
kubectl patch console.operator.openshift.io cluster \
  --type='json' \
  -p='[{"op": "add", "path": "/spec/plugins/-", "value": "mcp-gateway-console-plugin"}]'
```

### Manual Enablement

Visit the OpenShift Console:
1. Navigate to **Administration** → **Cluster Settings** → **Console** tab
2. Find "MCP Gateway Plugin" in the list
3. Click **Enable**
4. Refresh the browser after console operator reconciles

## Accessing the Plugin

Once enabled, the plugin adds a new section to the OpenShift Console:

1. Navigate to **Networking** → **MCP Gateway** (or wherever configured)
2. View the **Overview** dashboard showing server and tool statistics
3. Browse the **Servers** list to see registered MCP servers
4. Explore the **Tools** catalog to see available tools

## Configuration

### Image Override

To use a custom plugin image, patch the deployment:

```bash
kubectl -n mcp-system set image deployment/mcp-gateway-console-plugin \
  mcp-gateway-console-plugin=your-registry/mcp-gateway-console-plugin:custom-tag
```

### Replicas

The default is 2 replicas for high availability. To scale:

```bash
kubectl -n mcp-system scale deployment/mcp-gateway-console-plugin --replicas=3
```

### TLS Certificate

The plugin uses OpenShift's service-serving-cert mechanism. The certificate is automatically created and mounted by the annotation:

```yaml
annotations:
  service.beta.openshift.io/serving-cert-secret-name: mcp-gateway-console-plugin-cert
```

## Troubleshooting

### Plugin not showing in Console

1. Check if the ConsolePlugin resource exists:
   ```bash
   kubectl get consoleplugin mcp-gateway-console-plugin
   ```

2. Check if the plugin is enabled:
   ```bash
   kubectl get console.operator.openshift.io cluster -o jsonpath='{.spec.plugins}'
   ```

3. Check console operator logs:
   ```bash
   kubectl -n openshift-console-operator logs deployment/console-operator
   ```

### Plugin deployment failing

Check pod logs:
```bash
kubectl -n mcp-system logs -l app=mcp-gateway-console-plugin
```

Check events:
```bash
kubectl -n mcp-system get events --sort-by='.lastTimestamp'
```

### TLS certificate issues

Verify the service serving cert secret was created:
```bash
kubectl -n mcp-system get secret mcp-gateway-console-plugin-cert
```

If missing, ensure the service has the annotation and the service-ca controller is running.

## Uninstallation

To remove the console plugin:

```bash
# Disable from console
kubectl patch console.operator.openshift.io cluster \
  --type='json' \
  -p='[{"op": "remove", "path": "/spec/plugins", "value": ["mcp-gateway-console-plugin"]}]'

# Delete resources
kubectl delete consoleplugin mcp-gateway-console-plugin
kubectl -n mcp-system delete deployment,service,serviceaccount,configmap \
  -l app=mcp-gateway-console-plugin
```

## Development

For local development of the console plugin, see the [mcp-gateway-console-plugin repository](https://github.com/kuadrant/mcp-gateway-console-plugin).

## Architecture

The plugin consists of:
- **Frontend**: React application built with OpenShift Console SDK and PatternFly
- **Backend**: Nginx web server serving static assets over HTTPS
- **API Integration**: Calls the MCP Gateway broker's `/status` and `/mcp` endpoints

The plugin runs as a separate deployment and communicates with the broker service to fetch real-time data about MCP servers and tools.

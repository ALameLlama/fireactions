# Configure a container image registry mirror

A registry mirror caches images from another registry. Use a mirror that serves your prepared Fireactions guest image. The examples below use `mirror.example.com/fireactions/guest:ubuntu-24.04`. Replace that reference with the exact image reference served by your mirror.

Fireactions creates its own registry resolver. It does not read the host containerd registry mirror configuration. Changing that configuration alone does not route Fireactions pulls through a mirror.

Pull the mirror-qualified image into the configured containerd namespace with the devmapper snapshotter. The example namespace is `fireactions`. If your configuration uses a different namespace or containerd socket, change the `ctr` arguments to match:

```sh
sudo ctr --namespace fireactions images pull --snapshotter devmapper \
  mirror.example.com/fireactions/guest:ubuntu-24.04
sudo ctr --namespace fireactions images list
```

Alternatively, export the mirror-qualified image as an archive on a machine that can reach the mirror. Transfer the archive to the Fireactions host and import it into the same namespace:

```sh
docker pull mirror.example.com/fireactions/guest:ubuntu-24.04
docker save -o fireactions-guest.tar mirror.example.com/fireactions/guest:ubuntu-24.04
# On the Fireactions host:
sudo ctr --namespace fireactions images import --snapshotter devmapper fireactions-guest.tar
sudo ctr --namespace fireactions images list
```

Set each profile to the exact pulled or imported image reference and `image_pull_policy: Never`. For example, change these fields in an existing profile:

```yaml
containerd:
  address: /run/containerd/containerd.sock
  namespace: fireactions
pools:
  - name: ubuntu-24.04
    image: mirror.example.com/fireactions/guest:ubuntu-24.04
    image_pull_policy: Never
    # Keep the profile's remaining fields, including firecracker.
```

`Never` uses the image already stored in that namespace and does not contact a registry. A missing image fails provisioning. `Always`, or `IfNotPresent` with a missing image, invokes the Fireactions resolver instead. Make sure that the image list contains the exact configured reference before starting Fireactions.

A host mirror does not provide Docker actions or service containers inside workflow VMs. Fireactions does not start arbitrary workflow container images. A Docker registry mirror configured inside a guest or workflow has a separate purpose and does not change Fireactions image selection.

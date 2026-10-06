# Configure a container image registry mirror

A registry mirror is a host-side cache for container images. It can reduce repeated downloads from an upstream registry. This setup changes the host containerd configuration. Fireactions does not configure or replace containerd for you.

Choose a registry mirror that your host can reach. Configure containerd's registry host settings for the registry you want to mirror, using the configuration format for your installed containerd version. Preserve the existing containerd configuration and service. Do not replace either as part of a Fireactions install.

After you change host containerd configuration, follow your operating system's containerd procedure to check and apply it. Check that the host can reach the mirror and the upstream registry. Check that containerd can resolve and pull an image through the mirror.

Fireactions uses containerd to obtain configured guest root filesystem images. Its configured `containerd.namespace` controls where it looks for those images. Import guest image archives into that namespace when the host mirror is not used to pull them.

A host mirror does not provide Docker actions or service containers inside workflow VMs. Fireactions does not start arbitrary workflow container images. A Docker registry mirror configured inside a guest or workflow has a separate purpose and does not change Fireactions image selection.

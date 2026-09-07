# Optional bare-metal node pool for kata-containers based runners.
#
# Kata Containers boots a QEMU guest per pod and therefore needs hardware
# virtualisation. OCI VM shapes do not expose nested virtualisation, so this
# pool must use a BM.* shape (validated in variables.tf). Nodes register with
# the `cncf.io/kata-runner=true:NoSchedule` taint so only kata-deploy and kata
# runner workloads land on them, and kata-deploy adds
# `katacontainers.io/kata-runtime=true` once the runtime is installed.
locals {
  kata_node_taint = "cncf.io/kata-runner=true:NoSchedule"

  # Fail closed: if the OKE bootstrap script cannot be fetched or fails, the
  # node never joins rather than joining without the taint (which would let
  # ordinary workloads schedule onto a runner host).
  kata_node_cloud_init = <<-EOT
    #!/bin/bash
    set -euo pipefail
    curl --fail --silent --show-error --retry 5 --retry-delay 5 \
      -H "Authorization: Bearer Oracle" -L0 \
      http://169.254.169.254/opc/v2/instance/metadata/oke_init_script \
      | base64 --decode > /var/run/oke-init.sh
    test -s /var/run/oke-init.sh
    exec bash /var/run/oke-init.sh --kubelet-extra-args "--register-with-taints=${local.kata_node_taint}"
  EOT
}

resource "oci_containerengine_node_pool" "kata_worker" {
  count = var.kata_node_pool_enabled ? 1 : 0

  cluster_id     = oci_containerengine_cluster.service.id
  compartment_id = var.compartment_ocid

  kubernetes_version = var.nodepool_k8s_version
  name               = "${var.cluster_name}-kata-pool1"

  # BM shapes are fixed-size: no node_shape_config block.
  node_shape = var.kata_node_shape

  node_source_details {
    boot_volume_size_in_gbs = var.kata_node_boot_volume_size
    image_id                = local.non_gpu_images[0].image_id
    source_type             = "image"
  }

  node_metadata = {
    user_data = base64encode(local.kata_node_cloud_init)
  }

  initial_node_labels {
    key   = "cncf.io/kata-runner"
    value = "true"
  }

  node_config_details {
    size = var.kata_node_pool_size

    dynamic "placement_configs" {
      for_each = data.oci_identity_availability_domains.availability_domains.availability_domains
      content {
        availability_domain = placement_configs.value.name
        subnet_id           = oci_core_subnet.node.id
      }
    }

    node_pool_pod_network_option_details {
      cni_type          = "OCI_VCN_IP_NATIVE"
      pod_nsg_ids       = []
      pod_subnet_ids    = [oci_core_subnet.node.id]
      max_pods_per_node = 110
    }
  }

  node_pool_cycling_details {
    is_node_cycling_enabled = false
    maximum_unavailable     = 1
    maximum_surge           = 1
  }

  lifecycle {
    # The ClusterAutoscaler owns the pool size between min and max; a plan
    # after autoscaling must not resize the pool back to kata_node_pool_size.
    ignore_changes = [node_config_details[0].size]

    precondition {
      condition     = var.kata_node_pool_size >= var.kata_autoscaler_min && var.kata_node_pool_size <= var.kata_autoscaler_max
      error_message = "kata_node_pool_size must lie within [kata_autoscaler_min, kata_autoscaler_max]."
    }
  }
}

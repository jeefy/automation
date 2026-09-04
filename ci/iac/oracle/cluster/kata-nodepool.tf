# Optional bare-metal node pool for kata-containers based runners.
#
# Kata Containers requires either bare-metal hosts or nested virtualization.
# OCI VM shapes do not expose nested virtualization, so this pool must use a
# BM.* shape. Nodes register with the `cncf.io/kata-runner=true:NoSchedule`
# taint (via kubelet extra args in cloud-init) and matching node label, so
# only kata-deploy and kata runner workloads land on them.
locals {
  kata_node_cloud_init = <<-EOT
    #!/bin/bash
    curl --fail -H "Authorization: Bearer Oracle" -L0 http://169.254.169.254/opc/v2/instance/metadata/oke_init_script | base64 --decode >/var/run/oke-init.sh
    bash /var/run/oke-init.sh --kubelet-extra-args "--register-with-taints=cncf.io/kata-runner=true:NoSchedule"
  EOT
}

resource "oci_containerengine_node_pool" "kata_worker" {
  count = var.kata_node_pool_enabled ? 1 : 0

  cluster_id     = oci_containerengine_cluster.service.id
  compartment_id = var.compartment_ocid

  kubernetes_version = var.nodepool_k8s_version
  name               = "${var.cluster_name}-kata-pool1"

  node_shape = var.kata_node_shape

  # BM shapes are fixed-size; only Flex shapes take a shape config.
  dynamic "node_shape_config" {
    for_each = strcontains(var.kata_node_shape, "Flex") ? [1] : []
    content {
      memory_in_gbs = var.kata_node_memory
      ocpus         = var.kata_node_cpu
    }
  }

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
}

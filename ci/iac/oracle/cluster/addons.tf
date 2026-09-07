locals {
  # OKE ClusterAutoscaler add-on "nodes" format: "<min>:<max>:<nodepool-ocid>"
  # entries, comma separated for multiple pools (Oracle docs, "Working with the
  # Cluster Autoscaler as a Cluster Add-on").
  autoscaler_pools = concat(
    ["${var.cluster_autoscaler_min}:${var.cluster_autoscaler_max}:${oci_containerengine_node_pool.service_worker.id}"],
    var.kata_node_pool_enabled ? ["${var.kata_autoscaler_min}:${var.kata_autoscaler_max}:${oci_containerengine_node_pool.kata_worker[0].id}"] : [],
  )
}

resource "oci_containerengine_addon" "cluster_autoscaler" {
  addon_name                       = "ClusterAutoscaler"
  cluster_id                       = oci_containerengine_cluster.service.id
  remove_addon_resources_on_delete = true

  configurations {
    key   = "authType"
    value = "instance"
  }

  configurations {
    key   = "nodes"
    value = join(",", local.autoscaler_pools)
  }
}

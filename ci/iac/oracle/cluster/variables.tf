// Start: OCI provider Variables
variable "tenancy_ocid" {
  type        = string
  description = "The OCID of the Oracle Cloud Infrastructure tenancy where resources will be created."
}

variable "compartment_ocid" {
  type        = string
  description = "The OCID of the compartment in which OCI resources will be managed."
}

variable "user_ocid" {
  type        = string
  description = "The OCID of the OCI user associated with the API key used for authentication."
}

variable "private_key_path" {
  type        = string
  description = "Path to the private key file used for OCI API key authentication."
  sensitive   = true
}

variable "region" {
  type        = string
  description = "OCI region where resources will be deployed."
  default     = "us-sanjose-1"
}

variable "fingerprint" {
  type        = string
  description = "Fingerprint of the public key uploaded to the OCI user account for API authentication."
}

variable "config_file_profile" {
  type        = string
  description = "Profile name from the OCI CLI configuration file (~/.oci/config) to use for authentication."
  default     = "DEFAULT"
}

variable "oci_auth_type" {
  type        = string
  description = "Authentication method used by the OCI provider (e.g., APIKey, InstancePrincipal, ResourcePrincipal)."
  default     = "APIKey"
}
// End: OCI provider variables

variable "cluster_name" {
  type        = string
  description = "Name of the OKE cluster (also used for networking and other resources)"
}

variable "node_pool_worker_size" {
  type        = number
  description = "Default number of worker nodes"
}

variable "control_plane_k8s_version" {
  type        = string
  description = "Kubernetes version for the control plane"
}

variable "nodepool_k8s_version" {
  type        = string
  description = "Kubernetes version for OKE node pools"
}

variable "cluster_autoscaler_min" {
  type        = number
  description = "Minimum number of nodes for the cluster autoscaler"
}

variable "cluster_autoscaler_max" {
  type        = number
  description = "Maximum number of nodes for the cluster autoscaler"
}

variable "oke_node_shape" {
  type        = string
  description = "OKE Nodepool node shape"
}

variable "oke_node_memory" {
  type        = number
  description = "OKE worker node memory in GBs"
}

variable "oke_node_cpu" {
  type        = number
  description = "OKE worker node CPUs"
}

variable "oke_node_boot_volume_size" {
  type        = number
  description = "The size of the boot volume in GBs"
  default     = 50
}

variable "kata_node_pool_enabled" {
  type        = bool
  description = "Create a bare-metal node pool for kata-containers runners (pilot: oke-cncf-gha-phx only)."
  default     = false
}

variable "kata_node_pool_size" {
  type        = number
  description = "Initial number of nodes in the kata pool. After creation the ClusterAutoscaler owns the size (Terraform ignores drift); must be within [kata_autoscaler_min, kata_autoscaler_max]."
  default     = 1

  validation {
    condition     = var.kata_node_pool_size >= 0 && var.kata_node_pool_size <= 10 && floor(var.kata_node_pool_size) == var.kata_node_pool_size
    error_message = "kata_node_pool_size must be an integer between 0 and 10."
  }
}

variable "kata_autoscaler_min" {
  type        = number
  description = "ClusterAutoscaler minimum for the kata pool. 1 keeps one bare-metal host warm for the small runner reserve; 0 is only safe after a real cold-bootstrap test (kata-deploy must install before the first runner pod can start)."
  default     = 1

  validation {
    condition     = var.kata_autoscaler_min >= 0 && floor(var.kata_autoscaler_min) == var.kata_autoscaler_min
    error_message = "kata_autoscaler_min must be a non-negative integer."
  }
}

variable "kata_autoscaler_max" {
  type        = number
  description = "ClusterAutoscaler maximum for the kata pool. Each node is a whole bare-metal host; keep the pilot ceiling low."
  default     = 2

  validation {
    condition     = var.kata_autoscaler_max >= 1 && var.kata_autoscaler_max <= 10 && floor(var.kata_autoscaler_max) == var.kata_autoscaler_max
    error_message = "kata_autoscaler_max must be an integer between 1 and 10."
  }
}

variable "kata_node_shape" {
  type        = string
  description = "Shape for kata pool nodes. Must be a fixed-size bare-metal x86 shape (BM.Standard.*, BM.DenseIO.*, BM.Optimized.*); VM shapes lack nested virtualisation."
  default     = "BM.Standard.E4.128"

  validation {
    condition     = can(regex("^BM\\.(Standard|DenseIO|Optimized)[0-9]*\\.(E[0-9]+\\.)?[0-9]+$", var.kata_node_shape)) && !can(regex("\\.A[0-9]+\\.", var.kata_node_shape)) && !can(regex("Flex$", var.kata_node_shape))
    error_message = "kata_node_shape must be a fixed-size x86 bare-metal shape such as BM.Standard.E4.128 or BM.Standard3.64 (no VM.*, Flex or Ampere A1/A2 shapes)."
  }
}

variable "kata_node_boot_volume_size" {
  type        = number
  description = "Boot volume size in GBs for kata nodes. Hosts the containerd image cache (multi-GB runner image) and every runner pod's emptyDir scratch, so size it for max pods x scratch limit plus headroom."
  default     = 1024

  validation {
    condition     = var.kata_node_boot_volume_size >= 200 && var.kata_node_boot_volume_size <= 32768
    error_message = "kata_node_boot_volume_size must be between 200 and 32768 GB."
  }
}

variable "vcn_cidr" {
  type        = string
  description = "CIDR for the VCN"
}

variable "k8s_api_cidr" {
  type        = string
  description = "CIDR for the Kubernetes API network"
}

variable "svc_cidr" {
  type        = string
  description = "CIDR for the Service Network"
}

variable "node_cidr" {
  type        = string
  description = "CIDR for the worker nodes network"
}

variable "regional_service_cidr_label" {
  type        = string
  description = "The Service CIDR Labels follow a specific naming convention based on the regional key of the location"
}

variable "svc_lb_egress_rules" {
  description = "Egress security rules for the service LB security list"
  type = list(object({
    description      = string
    destination      = string
    destination_type = string
    protocol         = string
    stateless        = bool
    tcp_min          = optional(number)
    tcp_max          = optional(number)
    icmp_type        = optional(number)
    icmp_code        = optional(number)
  }))
  default = []
}

variable "svc_lb_ingress_rules" {
  description = "Ingress security rules for the service LB security list"
  type = list(object({
    description = string
    source      = string
    source_type = string
    protocol    = string
    stateless   = bool
    tcp_min     = optional(number)
    tcp_max     = optional(number)
    icmp_type   = optional(number)
    icmp_code   = optional(number)
  }))
  default = []
}

variable "k8s_api_endpoint_egress_rules" {
  description = "Egress security rules for the Kubernetes API Endpoint security list"
  type = list(object({
    description      = string
    destination      = string
    destination_type = string
    protocol         = string
    stateless        = bool
    tcp_min          = optional(number)
    tcp_max          = optional(number)
    icmp_type        = optional(number)
    icmp_code        = optional(number)
  }))
  default = []
}

variable "k8s_api_endpoint_ingress_rules" {
  description = "Ingress security rules for the Kubernetes API Endpoint security list"
  type = list(object({
    description = string
    source      = string
    source_type = string
    protocol    = string
    stateless   = bool
    tcp_min     = optional(number)
    tcp_max     = optional(number)
    icmp_type   = optional(number)
    icmp_code   = optional(number)
  }))
  default = []
}

variable "node_egress_rules" {
  description = "Egress security rules for the worker node security list"
  type = list(object({
    description      = string
    destination      = string
    destination_type = string
    protocol         = string
    stateless        = bool
    tcp_min          = optional(number)
    tcp_max          = optional(number)
    icmp_type        = optional(number)
    icmp_code        = optional(number)
  }))
  default = []
}

variable "node_ingress_rules" {
  description = "Ingress security rules for the worker node security list"
  type = list(object({
    description = string
    source      = string
    source_type = string
    protocol    = string
    stateless   = bool
    tcp_min     = optional(number)
    tcp_max     = optional(number)
    icmp_type   = optional(number)
    icmp_code   = optional(number)
  }))
  default = []
}

variable "deploy_ingress" {
  type        = bool
  description = "Deploy Ingress IP address"
  default     = false
}

variable "deploy_kcp" {
  type        = bool
  description = "Deploy KCP to the cluster and create LB IP for it"
  default     = false
}

variable "ingress_private_ip_id" {
  type    = string
  default = ""
}

variable "kcp_lb_private_ip_id" {
  type    = string
  default = ""
}

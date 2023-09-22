# Homer Reloader

Application to watch kubernetes Ingress and reload [Homer](https://github.com/bastienwirtz/homer) Configuration based on template and Ingress annotations.

By deafult application takes into account only Ingress resources labeled with `reloader.homer/enabled=true`. This can be changed by setting `--watch-all-ingresses` flag which will cause application to watch all Ingress resources in all namespaces.

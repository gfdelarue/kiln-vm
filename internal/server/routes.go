package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gfdelarue/kiln-vm/internal/api"
	"github.com/gfdelarue/kiln-vm/internal/auth"
	"github.com/gfdelarue/kiln-vm/internal/disk"
	"github.com/gfdelarue/kiln-vm/internal/metrics"
	mw "github.com/gfdelarue/kiln-vm/internal/middleware"
	"github.com/gfdelarue/kiln-vm/internal/version"
	vmservice "github.com/gfdelarue/kiln-vm/internal/vm"
)

func registerRoutes(r chi.Router, authService *auth.Service, vmService *vmservice.Service, diskService *disk.Service, metricsService *metrics.Service) {
	authHandler := api.NewAuthHandler(authService)
	vmHandler := api.NewVMHandler(vmService)
	diskHandler := api.NewDiskHandler(diskService)
	metricsHandler := api.NewMetricsHandler(metricsService)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": version.Version,
		})
	})

	r.Group(func(r chi.Router) {
		r.Use(mw.BearerAuth(authService))

		r.Post("/tokens", authHandler.CreateToken)
		r.Get("/tokens", authHandler.ListTokens)
		r.Delete("/tokens/{id}", authHandler.DeleteToken)

		r.Get("/vm-profiles", vmHandler.Profiles)
		r.Post("/vm-profiles/{profile}/prepare", vmHandler.PrepareProfile)
		r.Get("/vm-profile-images", vmHandler.ListProfileImages)
		r.Post("/vm-profile-images/ensure", vmHandler.EnsureProfileImage)
		r.Delete("/vm-profile-images/{id}", vmHandler.DeleteProfileImage)
		r.Delete("/vm-profile-images/profile/{profile}", vmHandler.DeleteProfileImagesByProfile)
		r.Post("/vm-images", vmHandler.CreateImage)
		r.Get("/vm-images", vmHandler.ListImages)
		r.Get("/vm-images/{ref}", vmHandler.GetImage)
		r.Delete("/vm-images/{ref}", vmHandler.DeleteImage)

		r.Post("/vms/plan", vmHandler.PlanCreate)
		r.Post("/vms", vmHandler.Create)
		r.Get("/vms", vmHandler.List)
		r.Get("/vms/{ref}", vmHandler.Get)
		r.Get("/vms/{ref}/metrics", metricsHandler.VM)
		r.Post("/vms/{ref}/start", vmHandler.Start)
		r.Post("/vms/{ref}/stop", vmHandler.Stop)
		r.Patch("/vms/{ref}/power", vmHandler.UpdatePower)
		r.Post("/vms/{ref}/verify", vmHandler.VerifyCapabilities)
		r.Delete("/vms/{ref}", vmHandler.Delete)
		r.Post("/vms/{ref}/exec", vmHandler.Exec)
		r.Put("/vms/{ref}/files", vmHandler.UploadFile)
		r.Get("/vms/{ref}/shell", vmHandler.Shell)
		r.Get("/vms/{ref}/forward/{port}", vmHandler.Forward)

		r.Get("/disk/usage", diskHandler.Usage)
		r.Post("/cleanup", diskHandler.Cleanup)

		r.Get("/metrics/summary", metricsHandler.Summary)
		r.Get("/node/capacity", metricsHandler.NodeCapacity)
		r.Get("/node/health", metricsHandler.NodeHealth)
	})
}

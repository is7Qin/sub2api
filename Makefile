.PHONY: build build-backend build-frontend test test-backend test-frontend test-frontend-critical test-admin-cli test-compose test-docker-deploy test-deploy-references test-install-contract secret-scan

FRONTEND_CRITICAL_VITEST := \
	src/views/auth/__tests__/LinuxDoCallbackView.spec.ts \
	src/views/auth/__tests__/WechatCallbackView.spec.ts \
	src/views/user/__tests__/PaymentView.spec.ts \
	src/views/user/__tests__/PaymentResultView.spec.ts \
	src/components/keys/__tests__/UseKeyModal.spec.ts \
	src/components/user/profile/__tests__/ProfileInfoCard.spec.ts \
	src/utils/__tests__/ccswitchImport.spec.ts \
	src/api/__tests__/channelMonitorV2.spec.ts \
	src/views/user/__tests__/ChannelStatusView.mode.spec.ts \
	src/views/admin/__tests__/SettingsView.spec.ts \
	src/features/channel-monitor-v2/__tests__/designSystem.structure.spec.ts \
	src/features/channel-monitor-v2/__tests__/monitorFormat.spec.ts \
	src/features/channel-monitor-v2/__tests__/monitorZoom.spec.ts

# 一键编译前后端
build: build-backend build-frontend

# 编译后端（复用 backend/Makefile）
build-backend:
	@$(MAKE) -C backend build

# 编译前端（需要已安装依赖）
build-frontend:
	@pnpm --dir frontend run build

# 运行测试（后端 + 前端 + admin CLI）
test: test-backend test-frontend test-admin-cli

test-backend:
	@$(MAKE) -C backend test

test-frontend:
	@pnpm --dir frontend run lint:check
	@pnpm --dir frontend run typecheck
	@$(MAKE) test-frontend-critical
	@sh deploy/test-caddyfile-cache.sh

test-frontend-critical:
	@pnpm --dir frontend exec vitest run $(FRONTEND_CRITICAL_VITEST)

test-admin-cli:
	@node --check skills/sub2api-admin/scripts/sub2api-admin.js
	@node --test skills/sub2api-admin/sub2api-admin.auth.test.js

test-compose:
	@$(MAKE) test-deploy-references
	@sh deploy/test-compose-config.sh
	@$(MAKE) test-docker-deploy
	@$(MAKE) test-install-contract

test-docker-deploy:
	@sh deploy/test-docker-deploy.sh

test-deploy-references:
	@sh deploy/test-deploy-references.sh

test-install-contract:
	@bash deploy/test-install-contract.sh

secret-scan:
	@python3 tools/secret_scan.py

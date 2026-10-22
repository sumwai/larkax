import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'

export default tseslint.config(
  { ignores: ['dist', 'node_modules'] },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      'react-refresh/only-export-components': [
        'warn',
        {
          allowConstantExport: true,
          allowExportNames: [
            'APP_NAV',
            'copyText',
            'exprPerMillion',
            'useAsync',
            'useAuth',
            'useDocumentTitle',
            'useI18n',
            'useIsMobile',
            'useIsNarrow',
            'useMediaQuery',
            'useReportPageTitle',
            'useSite',
            'useTheme',
            'useToast',
          ],
        },
      ],
    },
  },
  {
    files: ['vite.config.ts', 'playwright.config.ts', 'eslint.config.js', 'e2e/**/*.ts'],
    languageOptions: { globals: globals.node },
  },
)

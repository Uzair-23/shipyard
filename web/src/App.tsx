import { Route, Routes } from 'react-router-dom'
import Layout from './components/Layout.tsx'
import RequireAuth from './components/RequireAuth.tsx'
import Toasts from './components/Toasts.tsx'
import AppDetail from './pages/AppDetail.tsx'
import Apps from './pages/Apps.tsx'
import Login from './pages/Login.tsx'
import NewApp from './pages/NewApp.tsx'

export default function App() {
  return (
    <>
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route element={<RequireAuth />}>
          <Route element={<Layout />}>
            <Route path="/" element={<Apps />} />
            <Route path="/new" element={<NewApp />} />
            <Route path="/apps/:slug" element={<AppDetail />} />
          </Route>
        </Route>
      </Routes>
      <Toasts />
    </>
  )
}
